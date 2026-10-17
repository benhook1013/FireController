package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

func validCoverageSource(current SourcePlan) bool {
	observed, err := time.Parse(time.RFC3339Nano, current.ObservedAt)
	return err == nil && !observed.IsZero() && current.Repository.ID > 0 && repository.MatchString(current.Repository.FullName) && current.Repository.DefaultBranch != "" && safeText(current.Repository.DefaultBranch, 256) &&
		sameTarget(current.Pull, current.Repository.ID, current.Pull.Number) && current.Pull.Number > 0 && current.Pull.Number <= 2147483647 && current.Pull.State == "open" && !current.Pull.Merged && current.Pull.ChangedFiles > 0 &&
		current.Pull.BaseRef != "" && safeText(current.Pull.BaseRef, 256) && sha.MatchString(current.ParentSHA) && current.ParentSHA == current.Pull.BaseSHA && current.ParentPullNumber >= 0 && current.ParentPullNumber <= 2147483647 && current.ParentPullNumber != current.Pull.Number &&
		(current.ParentPullNumber != 0 || current.Pull.BaseRef == current.Repository.DefaultBranch)
}

// ComparisonState separates applicable closed coverage from unresolved native
// evidence on any head in the selected channel. Pending dominates admission
// even when Covered is true.
type ComparisonState struct {
	Covered bool
	Pending bool
}

// Coverage reports usable current coverage only when no completed native
// operation in the selected channel on this PR still needs capture, adjudication,
// finalization or proof.
func (e *Engine) Coverage(ctx context.Context, project string, current SourcePlan, channel string) (bool, error) {
	state, err := e.Comparison(ctx, project, current, channel)
	return state.Covered && !state.Pending, err
}

// Comparison reads indexed native proof facts without provider work.
// Callers supply fresh comparison facts; imported browser history is not credit.
func (e *Engine) Comparison(ctx context.Context, project string, current SourcePlan, channel string) (ComparisonState, error) {
	if channel != "cli" && channel != "hosted" || !identifier.MatchString(project) || !validCoverageSource(current) {
		return ComparisonState{}, invalid()
	}
	var result ComparisonState
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != current.Repository.ID {
			return sourceHold("repository_changed")
		}
		anchor := Anchor{RepositoryID: repositoryID, PullNumber: current.Pull.Number, CandidateSHA: current.Pull.HeadSHA, BaseSHA: current.Pull.BaseSHA, ParentSHA: current.ParentSHA, ParentPullNumber: current.ParentPullNumber, BaseRef: current.Pull.BaseRef}
		var err error
		result, err = comparison(ctx, tx, project, repositoryID, current.Pull.Number, anchor, channel)
		return err
	})
	if err != nil {
		return ComparisonState{}, err
	}
	return result, nil
}

// comparison reads indexed native proof facts. Pending and dirty obligations
// apply across every retained head; exact comparison credit is separate.
func comparison(ctx context.Context, tx *sql.Tx, project string, repositoryID, number int64, current Anchor, channel string) (ComparisonState, error) {
	if channel != "cli" && channel != "hosted" {
		return ComparisonState{}, invalid()
	}
	var dirty bool
	var result ComparisonState
	err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='dirty'),
 EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='pending'),
 EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='ready' AND repository_id=? AND candidate_sha=? AND base_sha=? AND parent_sha=? AND parent_pull_number=? AND base_ref=?)`,
		project, number, channel, project, number, channel, project, number, channel, repositoryID, current.CandidateSHA, current.BaseSHA, current.ParentSHA, current.ParentPullNumber, current.BaseRef).Scan(&dirty, &result.Pending, &result.Covered)
	if err != nil {
		return ComparisonState{}, err
	}
	if dirty {
		return ComparisonState{}, adjudicationStorageError()
	}
	return result, nil
}

func coverageProof(ctx context.Context, tx EvidenceQuery, evidence ReviewEvidence, state ReviewAdjudication, channel string) (bool, error) {
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
			ready = ready && resolved
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
