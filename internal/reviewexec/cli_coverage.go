package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func validCoverageSource(current SourcePlan) bool {
	observed, err := time.Parse(time.RFC3339Nano, current.ObservedAt)
	return err == nil && !observed.IsZero() && current.SourceReason == "" && current.Repository.ID > 0 && repository.MatchString(current.Repository.FullName) && current.Repository.DefaultBranch != "" && safeText(current.Repository.DefaultBranch, 256) &&
		sameTarget(current.Pull, current.Repository.ID, current.Pull.Number) && current.Pull.Number > 0 && current.Pull.Number <= 2147483647 && current.Pull.State == "open" && !current.Pull.Merged && current.Pull.ChangedFiles > 0 &&
		current.Pull.BaseRef != "" && safeText(current.Pull.BaseRef, 256) && sha.MatchString(current.ParentSHA) && current.ParentSHA == current.Pull.BaseSHA && current.ParentPullNumber >= 0 && current.ParentPullNumber <= 2147483647 && current.ParentPullNumber != current.Pull.Number &&
		(current.ParentPullNumber != 0 || current.Pull.BaseRef == current.Repository.DefaultBranch)
}

// ComparisonState separates completed PR/channel review progress from unresolved
// native evidence on any head. Covered does not claim that later code was reviewed;
// Pending dominates admission even when a prior review is complete.
type ComparisonState struct {
	Covered          bool
	Pending          bool
	ExternalPending  bool
	RoundsConfigured bool
	RoundsComplete   bool
}

// Coverage reports completed PR/channel review progress only when no native
// operation in the selected channel on this PR still needs capture, adjudication,
// finalization or proof.
func (e *Engine) Coverage(ctx context.Context, project string, current SourcePlan, channel string) (bool, error) {
	state, err := e.Comparison(ctx, project, current, channel)
	return state.Covered && !state.Pending, err
}

// Comparison reads indexed native proof facts without provider work.
// Callers supply selected PR identity; imported browser history is not credit.
// Source freshness is an admission guard, not a reset of completed review progress.
func (e *Engine) Comparison(ctx context.Context, project string, current SourcePlan, channel string) (ComparisonState, error) {
	if channel != "cli" && channel != "hosted" || !identifier.MatchString(project) || !validCoverageSource(current) {
		return ComparisonState{}, invalid()
	}
	var result ComparisonState
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := requireSourceObligationsClear(ctx, tx, project, current.Pull.Number); err != nil {
			return err
		}
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != current.Repository.ID {
			return sourceHold("repository_changed")
		}
		var err error
		result, err = channelProgress(ctx, tx, project, repositoryID, current.Pull.Number, channel)
		return err
	})
	if err != nil {
		return ComparisonState{}, err
	}
	return result, nil
}

// Retained partial findings are obligations even after execution releases its
// reservation. Use existing operation/evidence indexes, independently of the
// completed-round proof ledger and the currently published browser snapshot.
const partialCLIResults = ` FROM review_operations o
 CROSS JOIN review_execution_evidence e ON e.project_id=o.project_id AND e.attempt_id=o.attempt_id AND e.operation_id=o.id
 WHERE o.project_id=? AND o.pull_number=? AND o.channel='cli' AND o.state='failed'
 AND json_extract(e.body_json,'$.capture.review.Complete')=0`

// Findings survive every known terminal CLI process outcome. Keep the narrower
// failed-only comparison suppression separate from these durable obligations.
const retainedCLIResults = ` FROM review_operations o
 CROSS JOIN review_execution_evidence e ON e.project_id=o.project_id AND e.attempt_id=o.attempt_id AND e.operation_id=o.id
 WHERE o.project_id=? AND o.pull_number=? AND o.channel='cli' AND o.state IN ('failed','cancelled','rate_limited')
 AND json_extract(e.body_json,'$.capture.review.Complete')=0`

const partialCLIPending = ` AND json_array_length(e.body_json,'$.capture.review.Findings')>0
 AND (NOT EXISTS(SELECT 1 FROM review_adjudication_state s WHERE s.project_id=e.project_id AND s.evidence_id=e.id)
 OR EXISTS(SELECT 1 FROM review_adjudication_state s, json_each(s.body_json,'$.findings') f
 WHERE s.project_id=e.project_id AND s.evidence_id=e.id
 AND (json_extract(f.value,'$.disposition')='unadjudicated' OR
 (json_extract(f.value,'$.disposition')='accepted' AND json_extract(f.value,'$.fix_sha')=''))))`

// Compare the admitted Git identities. A change in scheduling or adjudication
// alone must not spend another automatic CLI run on the same partial input.
func partialCLIComparisonExists(ctx context.Context, q EvidenceQuery, project string, repo, number int64, head, base, ref, parent string, parentNumber int64) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+partialCLIResults+`
 AND (o.reason IN('coverage_incomplete','cli_partial_settled') OR json_array_length(e.body_json,'$.capture.review.Findings')>0
 OR (o.reason='capture_retained' AND json_extract(e.body_json,'$.capture.review.Outcome')='failed'
 AND json_extract(e.body_json,'$.capture.review.coverage_limits[0]')=?))
 AND o.head_sha=? AND json_extract(e.body_json,'$.anchor.repository_id')=?
 AND json_extract(e.body_json,'$.anchor.base_sha')=? AND json_extract(e.body_json,'$.anchor.base_ref')=?
 AND json_extract(e.body_json,'$.anchor.parent_sha')=? AND json_extract(e.body_json,'$.anchor.parent_pull_number')=?)`,
		project, number, reviewprovider.CLIUnreviewedFilesLimit, head, repo, base, ref, parent, parentNumber).Scan(&exists)
	return exists, err
}

const (
	comparisonDirtyExists   = `EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='dirty')`
	comparisonPendingExists = `EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='pending')`
	comparisonReadyExists   = `EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='ready' AND repository_id=?)`
)

// channelProgress reads completed native PR/channel outcomes. Captured anchors
// remain evidence provenance; later commits cannot reopen earned progress.
// Pending and dirty obligations still apply across every retained head.
func channelProgress(ctx context.Context, tx *sql.Tx, project string, repositoryID, number int64, channel string) (ComparisonState, error) {
	if channel != "cli" && channel != "hosted" {
		return ComparisonState{}, invalid()
	}
	var dirty bool
	var result ComparisonState
	err := tx.QueryRowContext(ctx, "SELECT "+comparisonDirtyExists+", "+comparisonPendingExists+", "+comparisonReadyExists,
		project, number, channel, project, number, channel, project, number, channel, repositoryID).Scan(&dirty, &result.Pending, &result.Covered)
	if err != nil {
		return ComparisonState{}, err
	}
	if dirty {
		return ComparisonState{}, &automaticProjectStateError{adjudicationStorageError()}
	}
	if channel == "cli" && !result.Pending {
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+retainedCLIResults+partialCLIPending+")", project, number).Scan(&result.Pending); err != nil {
			return ComparisonState{}, err
		}
	}
	if channel == "hosted" {
		ids, err := reviews.PendingExternalHosted(ctx, tx, project, repositoryID, number, 1)
		if err != nil {
			return ComparisonState{}, err
		}
		result.ExternalPending = len(ids) > 0
		result.Pending = result.Pending || result.ExternalPending
		if !result.Covered {
			// A deliberately counted and fixed batch supplies earned channel
			// coverage for its original repository, across later head movement.
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM review_round_external_credits credit
 JOIN review_round_external_members member ON member.batch_id=credit.batch_id AND member.project_id=credit.project_id
 JOIN review_run_versions v ON v.project_id=member.project_id AND v.id=member.run_id AND v.pull_number=credit.pull_number AND v.finalized=1
 JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
 WHERE credit.project_id=? AND credit.pull_number=? AND s.repository_id=?)`, project, number, repositoryID).Scan(&result.Covered)
			if err != nil {
				return ComparisonState{}, err
			}
		}
	}
	rounds, err := readRoundControl(ctx, tx, project, number, channel)
	if err != nil {
		return ComparisonState{}, err
	}
	result.RoundsConfigured = rounds.Configured
	result.RoundsComplete = rounds.Configured && (rounds.Closed || roundDone(rounds))
	return result, nil
}

func coverageProof(ctx context.Context, tx EvidenceQuery, evidence ReviewEvidence, state ReviewAdjudication, channel string, requireReceivingFix bool) (bool, error) {
	if channel != "cli" && channel != "hosted" || channel == "cli" && evidence.Capture.Hosted != nil || channel == "hosted" && validateHostedEvidenceMetadata(evidence.Capture.Hosted, evidence.Anchor, evidence.Capture.Review) != nil {
		return false, adjudicationStorageError()
	}
	complete := evidence.Capture.Review.Complete && evidence.Capture.Review.Outcome == "completed"
	if state.Finalized {
		counts := state.FrozenCounts
		if !complete || counts == nil || counts.Found != int64(len(evidence.Capture.Review.Findings)) || counts.Accepted < 0 || counts.Routed < 0 || counts.Accepted > counts.Found-counts.Routed {
			return false, adjudicationStorageError()
		}
	}
	ready := complete && state.Finalized
	for _, finding := range state.Findings {
		if finding.Disposition != "routed" && finding.RouteID != "" {
			return false, adjudicationStorageError()
		}
		if finding.Disposition == "unadjudicated" {
			if state.Finalized || finding.Reason != "" || finding.ActorID != "" || finding.DecidedAt != "" || finding.FixSHA != "" || finding.ProofNote != "" || finding.FixedAt != "" {
				return false, adjudicationStorageError()
			}
			ready = false
			continue
		}
		decided, err := time.Parse(time.RFC3339Nano, finding.DecidedAt)
		if strings.TrimSpace(finding.Reason) == "" || !identifier.MatchString(finding.ActorID) || err != nil || decided.IsZero() {
			return false, adjudicationStorageError()
		}
		if finding.Disposition == "routed" {
			route, err := projectedFindingRoute(ctx, tx, evidence, finding)
			if err != nil {
				return false, err
			}
			resolved, err := receivingRouteResolved(route)
			if err != nil {
				return false, err
			}
			if requireReceivingFix {
				ready = ready && resolved
			}
			continue
		}
		if finding.FixSHA == "" {
			if finding.ProofNote != "" || finding.FixedAt != "" {
				return false, adjudicationStorageError()
			}
			if finding.Disposition == "accepted" {
				ready = false
			}
			continue
		}
		fixed, err := time.Parse(time.RFC3339Nano, finding.FixedAt)
		if finding.Disposition != "accepted" || !sha.MatchString(finding.FixSHA) || strings.TrimSpace(finding.ProofNote) == "" || err != nil || fixed.IsZero() {
			return false, adjudicationStorageError()
		}
	}
	return ready, nil
}
