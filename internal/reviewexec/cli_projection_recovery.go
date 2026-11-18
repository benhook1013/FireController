package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"slices"
)

// RecoverInvalidCLIProjection is a trusted in-process reparse boundary, not an
// upload or operator API. The caller must reread the original retained capture,
// verify its digest and owned comparison, and prove full changed-file coverage.
// It archives the exact empty fallback before correcting only its projection.
// No process/provider request, completion, credit or reservation release occurs.
func (e *Engine) RecoverInvalidCLIProjection(ctx context.Context, project, attemptID string, expectedRevision int64, input ReviewEvidenceInput) (Receipt, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) || input.Hosted != nil || !input.Review.Complete || input.Review.Outcome != "completed" {
		return Receipt{}, invalid()
	}
	if err := validateReviewEvidence(input); err != nil {
		return Receipt{}, err
	}
	if err := validateNewCoverageLimits(input); err != nil {
		return Receipt{}, err
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(canonical) > evidenceInputLimit {
		return Receipt{}, invalid()
	}
	digest := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(digest[:])
	var receipt Receipt
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		var id, body, prior string
		if err = tx.QueryRowContext(ctx, `SELECT id,body_json,fingerprint FROM review_execution_evidence WHERE project_id=? AND attempt_id=?`, project, attemptID).Scan(&id, &body, &prior); err != nil {
			return err
		}
		// Exact replay never reopens a completed operation or repeats an audit.
		if prior == fingerprint {
			receipt = Receipt{ID: id, State: "captured", Revision: 1}
			return nil
		}
		if a.Revision != expectedRevision || a.State != "uncertain" || o.State != a.State || o.Reason != "capture_invalid" || o.Channel != "cli" || o.AttemptID != a.ID || a.Intent != "" || a.ExternalID != "" || a.Anchor == nil || *a.Anchor != input.ExpectedAnchor {
			return conflict()
		}
		var configurationBody string
		if err = tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, project, o.ConfigurationRevision).Scan(&configurationBody); err != nil {
			return err
		}
		var configuration Configuration
		if len(configurationBody) > 32<<10 || json.Unmarshal([]byte(configurationBody), &configuration) != nil || validateConfigurationShape(configuration) != nil || encoded(configuration) != configurationBody || *configuration.ExpectedRevision != o.ConfigurationRevision-1 || configuration.Paused || !slices.Contains(configuration.Channels, "cli") || !slices.Contains(configuration.Queue, o.PullNumber) {
			return conflict()
		}
		capability, ok := e.capabilities[configuration.CapabilityID]
		if !ok || capability.Fingerprint != o.CapabilityFingerprint || capability.Provider != a.Provider || capability.Account != a.Account || !slices.Contains(capability.Channels, "cli") {
			return conflict()
		}
		old, state, err := loadReviewAdjudication(ctx, tx, project, id)
		if err != nil {
			return err
		}
		oldDigest := sha256.Sum256([]byte(encoded(old.Capture)))
		if encoded(old) != body || hex.EncodeToString(oldDigest[:]) != prior || old.Recovery != nil || old.ProjectID != project || old.AttemptID != a.ID || old.OperationID != o.ID || old.CapabilityFingerprint != o.CapabilityFingerprint || old.Anchor != *a.Anchor || old.Capture.ExpectedAnchor != *a.Anchor || old.Capture.Hosted != nil {
			return conflict()
		}
		r := old.Capture.Review
		if r.Complete || r.Outcome != "uncertain" || len(r.Findings) != 0 || r.ReportedFindings != 0 || len(r.ReviewedFiles) != 0 || len(r.CoverageLimits) != 0 {
			return conflict()
		}
		// Only interpretation can change. All original source and timing facts stay.
		original := old.Capture
		original.Review = input.Review
		if encoded(original) != encoded(input) {
			return conflict()
		}
		var repositoryID int64
		var reserved bool
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND action_class='cli' AND repository_id=? AND pull_number=?)`, a.ID, a.Provider, a.Account, repositoryID, o.PullNumber).Scan(&reserved); err != nil {
			return err
		}
		if !reserved || repositoryID != a.Anchor.RepositoryID || o.HeadSHA != a.Anchor.CandidateSHA || o.PullNumber != a.Anchor.PullNumber {
			return conflict()
		}
		for _, q := range []string{
			`SELECT count(*) FROM review_adjudication_state WHERE evidence_id=?`,
			`SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`,
			`SELECT count(*) FROM review_finding_routes WHERE source_evidence_id=?`,
			`SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`,
			`SELECT count(*) FROM review_applicability WHERE evidence_id=?`,
			`SELECT count(*) FROM review_cli_evidence_recovery WHERE evidence_id=?`,
			`SELECT count(*) FROM review_round_completions WHERE evidence_id=?`,
		} {
			var n int
			if err = tx.QueryRowContext(ctx, q, id).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return conflict()
			}
		}
		oldRun, err := publicationRun(old, state, a)
		if err != nil {
			return err
		}
		_, published, err := publicationReplay(ctx, tx, project, o.PullNumber, oldRun)
		if err != nil {
			return err
		}
		recoveredAt := stamp()
		next := old
		next.Capture = input
		next.Recovery = &EvidenceRecovery{PriorFingerprint: prior, RecoveredAt: recoveredAt, Reason: "cli_capture_reparsed"}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_cli_evidence_recovery(evidence_id,project_id,original_body_json,original_fingerprint,recovered_fingerprint,recovered_at) VALUES(?,?,?,?,?,?)`, id, project, body, prior, fingerprint, recoveredAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=? AND fingerprint=?`, encoded(next), fingerprint, id, prior); err != nil {
			return err
		}
		if published {
			_, recoveredState, stateErr := loadReviewAdjudication(ctx, tx, project, id)
			if stateErr != nil {
				return stateErr
			}
			newRun, runErr := publicationRun(next, recoveredState, a)
			if runErr != nil {
				return runErr
			}
			if _, err = reviews.RecoverNativeCLIProjection(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, o.PullNumber, oldRun, newRun); err != nil {
				return err
			}
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.cli_capture_reparsed", id, 1, struct{ AttemptID, PriorFingerprint, RecoveredFingerprint string }{a.ID, prior, fingerprint}); err != nil {
			return err
		}
		receipt = Receipt{ID: id, State: "captured", Revision: 1}
		return nil
	})
	return receipt, err
}
