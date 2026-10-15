package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
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

// Comparison reads bounded completed native history without provider work.
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

// comparison is shared with the final action guard in its existing snapshot.
// Bound the all-head completed-operation scan before loading any capture body.
func comparison(ctx context.Context, tx *sql.Tx, project string, repositoryID, number int64, current Anchor, channel string) (ComparisonState, error) {
	var result ComparisonState
	if channel != "cli" && channel != "hosted" {
		return result, invalid()
	}
	// Bound completed operations before any evidence lookup. Failed or
	// cancelled history must not expand this read's scan.
	rows, err := tx.QueryContext(ctx, `SELECT id,attempt_id FROM review_operations
   WHERE project_id=? AND pull_number=? AND channel=? AND state='completed'
   ORDER BY id LIMIT 101`, project, number, channel)
	if err != nil {
		return ComparisonState{}, err
	}
	type candidate struct{ operation, attempt string }
	candidates := make([]candidate, 0, 101)
	for rows.Next() {
		var next candidate
		if err = rows.Scan(&next.operation, &next.attempt); err != nil {
			break
		}
		candidates = append(candidates, next)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return ComparisonState{}, err
	}
	if len(candidates) > 100 {
		return ComparisonState{}, core.Fail("review_capacity", "The selected pull request has too many completed operations in the selected channel for a bounded coverage check; inspect its history before requesting another review.", 409)
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ComparisonState{}, ctx.Err()
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,attempt_id FROM review_execution_evidence WHERE project_id=? AND operation_id=? ORDER BY id LIMIT 2`, project, candidate.operation)
		if err != nil {
			return ComparisonState{}, err
		}
		var evidenceID, evidenceAttempt string
		found := 0
		for rows.Next() {
			if err = rows.Scan(&evidenceID, &evidenceAttempt); err != nil {
				break
			}
			found++
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return ComparisonState{}, err
		}
		if found == 0 {
			result.Pending = true
			continue
		}
		if found != 1 || evidenceAttempt != candidate.attempt {
			return ComparisonState{}, adjudicationStorageError()
		}
		evidence, state, err := loadReviewAdjudication(ctx, tx, project, evidenceID)
		if err != nil {
			return ComparisonState{}, err
		}
		attempt, err := readAttempt(ctx, tx, project, candidate.attempt)
		if err != nil {
			return ComparisonState{}, err
		}
		operation, err := readOperation(ctx, tx, project, candidate.operation)
		if err != nil {
			return ComparisonState{}, err
		}
		if validateReviewEvidence(evidence.Capture) != nil || !identifier.MatchString(evidence.ID) || !identifier.MatchString(evidence.AttemptID) || !identifier.MatchString(evidence.OperationID) || !identifier.MatchString(operation.ActorID) || operation.WorkerID != "" && !identifier.MatchString(operation.WorkerID) || evidence.AttemptID != attempt.ID || evidence.OperationID != operation.ID || attempt.OperationID != operation.ID || operation.AttemptID != attempt.ID || operation.Channel != channel || operation.State != "completed" || attempt.State != "completed" || operation.ConfigurationRevision < 1 ||
			!identifier.MatchString(attempt.Provider) || !identifier.MatchString(attempt.Account) || !hash.MatchString(evidence.CapabilityFingerprint) || evidence.CapabilityFingerprint != operation.CapabilityFingerprint || attempt.Anchor == nil || !validEvidenceAnchor(evidence.Anchor) || *attempt.Anchor != evidence.Anchor || evidence.Capture.ExpectedAnchor != evidence.Anchor || operation.PullNumber != evidence.Anchor.PullNumber || operation.HeadSHA != evidence.Anchor.CandidateSHA || evidence.Anchor.RepositoryID != repositoryID {
			return ComparisonState{}, adjudicationStorageError()
		}
		if channel == "hosted" && validateHostedEvidenceIdentity(attempt, operation, evidence.Capture) != nil {
			return ComparisonState{}, adjudicationStorageError()
		}
		ready, err := coverageProof(evidence, state, channel)
		if err != nil {
			return ComparisonState{}, err
		}
		if !ready {
			result.Pending = true
		}
		anchor := evidence.Anchor
		// Canonical names can change while stable repository and Git identities do
		// not. Names remain validated above, but are not coverage identity keys.
		if ready && anchor.RepositoryID == current.RepositoryID && anchor.PullNumber == number && anchor.CandidateSHA == current.CandidateSHA && anchor.BaseSHA == current.BaseSHA && anchor.ParentSHA == current.ParentSHA && anchor.ParentPullNumber == current.ParentPullNumber && anchor.BaseRef == current.BaseRef {
			result.Covered = true
		}
	}
	return result, nil
}

func coverageProof(evidence ReviewEvidence, state ReviewAdjudication, channel string) (bool, error) {
	if channel != "cli" && channel != "hosted" || channel == "cli" && evidence.Capture.Hosted != nil || channel == "hosted" && validateHostedEvidenceMetadata(evidence.Capture.Hosted, evidence.Anchor, evidence.Capture.Review) != nil {
		return false, adjudicationStorageError()
	}
	complete := evidence.Capture.Review.Complete && evidence.Capture.Review.Outcome == "completed"
	if state.Finalized {
		counts := state.FrozenCounts
		if !complete || counts == nil || counts.Found != int64(len(evidence.Capture.Review.Findings)) || counts.Accepted < 0 || counts.Accepted > counts.Found || counts.Routed != 0 {
			return false, adjudicationStorageError()
		}
	}
	ready := complete && state.Finalized
	for _, finding := range state.Findings {
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
