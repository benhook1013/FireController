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

// CLICoverage checks one channel's retained source and current local proof. It
// neither allocates coverage nor establishes policy, CI or merge readiness.
// The caller owns fresh source observation and current dispatch authorization.
func (e *Engine) CLICoverage(ctx context.Context, project string, current SourcePlan) (bool, error) {
	if !identifier.MatchString(project) || !validCoverageSource(current) {
		return false, invalid()
	}
	covered := false
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != current.Repository.ID {
			return sourceHold("repository_changed")
		}
		// Bound completed operations before any evidence lookup. Failed or
		// cancelled same-head history must not expand this read's scan.
		rows, err := tx.QueryContext(ctx, `SELECT id,attempt_id FROM review_operations
   WHERE project_id=? AND pull_number=? AND channel='cli' AND head_sha=? AND state='completed'
   ORDER BY id LIMIT 101`, project, current.Pull.Number, current.Pull.HeadSHA)
		if err != nil {
			return err
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
			return err
		}
		if len(candidates) > 100 {
			return core.Fail("review_capacity", "The selected comparison has too many completed CLI operations for a bounded coverage check; inspect its history before requesting another review.", 409)
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rows, err := tx.QueryContext(ctx, `SELECT id,attempt_id FROM review_execution_evidence WHERE project_id=? AND operation_id=? ORDER BY id LIMIT 2`, project, candidate.operation)
			if err != nil {
				return err
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
				return err
			}
			if found == 0 {
				continue
			}
			if found != 1 || evidenceAttempt != candidate.attempt {
				return adjudicationStorageError()
			}
			evidence, state, err := loadCLIAdjudication(ctx, tx, project, evidenceID)
			if err != nil {
				return err
			}
			attempt, err := readAttempt(ctx, tx, project, candidate.attempt)
			if err != nil {
				return err
			}
			operation, err := readOperation(ctx, tx, project, candidate.operation)
			if err != nil {
				return err
			}
			if validateCLIEvidence(evidence.Capture) != nil || !identifier.MatchString(evidence.ID) || !identifier.MatchString(evidence.AttemptID) || !identifier.MatchString(evidence.OperationID) || !identifier.MatchString(operation.ActorID) || operation.WorkerID != "" && !identifier.MatchString(operation.WorkerID) || evidence.AttemptID != attempt.ID || evidence.OperationID != operation.ID || attempt.OperationID != operation.ID || operation.AttemptID != attempt.ID || operation.Channel != "cli" || operation.State != "completed" || attempt.State != "completed" || operation.ConfigurationRevision < 1 ||
				!identifier.MatchString(attempt.Provider) || !identifier.MatchString(attempt.Account) || !hash.MatchString(evidence.CapabilityFingerprint) || evidence.CapabilityFingerprint != operation.CapabilityFingerprint || attempt.Anchor == nil || !validEvidenceAnchor(evidence.Anchor) || *attempt.Anchor != evidence.Anchor || evidence.Capture.ExpectedAnchor != evidence.Anchor || operation.PullNumber != evidence.Anchor.PullNumber || operation.HeadSHA != evidence.Anchor.CandidateSHA {
				return adjudicationStorageError()
			}
			ready, err := cliCoverageProof(evidence, state)
			if err != nil {
				return err
			}
			anchor := evidence.Anchor
			// Canonical names can change while stable repository and Git identities do
			// not. Names remain validated above, but are not coverage identity keys.
			if ready && anchor.RepositoryID == current.Repository.ID && anchor.PullNumber == current.Pull.Number && anchor.CandidateSHA == current.Pull.HeadSHA && anchor.BaseSHA == current.Pull.BaseSHA && anchor.ParentSHA == current.ParentSHA && anchor.ParentPullNumber == current.ParentPullNumber && anchor.BaseRef == current.Pull.BaseRef {
				covered = true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return covered, nil
}

func cliCoverageProof(evidence CLIEvidence, state CLIAdjudication) (bool, error) {
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
