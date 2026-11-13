package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const HostedFinishedIncompleteReason = "hosted_finished_incomplete"
const HostedIncompleteGrace = 2 * time.Minute

// HostedIncompleteCapture identifies the latest immutable source reinterpreted
// by the trusted observer. It is not an uploaded claim or a public control.
type HostedIncompleteCapture struct {
	CaptureID string
	SHA256    string
	Bytes     int64
}

// ReleaseFinishedHosted ends an already-issued request without review evidence
// or credit. The trusted runtime calls this only after a fresh complete provider
// collection again proves the exact command finished but its inventory remains
// unproven. Age alone never invokes this path, and it cannot post another review.
func (e *Engine) ReleaseFinishedHosted(ctx context.Context, project, attemptID string, revision int64, source HostedIncompleteCapture) (Attempt, error) {
	if ctx == nil || e == nil || !hash.MatchString(source.SHA256) || source.CaptureID != "review_capture_"+source.SHA256[:32] || source.Bytes <= 0 || source.Bytes > 48<<20 {
		return Attempt{}, invalid()
	}
	return e.advanceGuarded(ctx, project, attemptID, revision, "failed", HostedFinishedIncompleteReason, "", nil, func(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) error {
		if a.State != "uncertain" || o.State != a.State || o.Reason != "hosted_finished_inventory_unproven" || o.AttemptID != a.ID || o.Channel != "hosted" {
			return conflict()
		}
		if _, _, err := readHostedPostingAttempt(ctx, tx, project, a.ID); err != nil {
			return err
		}
		if err := validateConfirmedHostedSettlement(ctx, tx, a, o); err != nil {
			return err
		}
		var state, reason, external, firstObserved string
		if err := tx.QueryRowContext(ctx, `SELECT state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&state, &reason, &external, &firstObserved); err != nil {
			return err
		}
		first, valid := attemptRecordedTime(firstObserved)
		if !valid || state != a.State || reason != o.Reason || external != a.ExternalID {
			return hostedRecoveryUnavailable()
		}
		if err := validateSettlementAdmission(ctx, tx, a, o, firstObserved); err != nil {
			return err
		}
		now := e.sampleAttemptTime().UTC()
		if now.Before(first.Add(HostedIncompleteGrace)) {
			return core.Fail("hosted_incomplete_grace", "The finished review is still within its bounded inventory grace period.", 409)
		}
		// Validate the entire original journal, then bind the fresh decoder result
		// to its latest record. A concurrent newer capture must be reinterpreted.
		if err := validateSettlementObservations(ctx, tx, a, o, false); err != nil {
			return err
		}
		var id, rowProject, rowAttempt, operation, checksum, body string
		var position, bytes int64
		if err := tx.QueryRowContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,body_json FROM review_hosted_observations WHERE attempt_id=? ORDER BY position DESC LIMIT 1`, a.ID).Scan(&id, &rowProject, &rowAttempt, &operation, &position, &checksum, &bytes, &body); err != nil {
			return err
		}
		record, err := decodeHostedObservation(body, id, rowProject, rowAttempt, operation, position, checksum, bytes, a, o)
		if err != nil {
			return err
		}
		if rowProject != project || rowAttempt != a.ID || operation != o.ID || record.Input.Kind != "" || record.Input.CaptureID != source.CaptureID || checksum != source.SHA256 || bytes != source.Bytes {
			return conflict()
		}
		var evidence bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?)`, a.ID).Scan(&evidence); err != nil {
			return err
		}
		if evidence {
			return conflict()
		}
		var provider, account, class string
		var repository, pull int64
		err = tx.QueryRowContext(ctx, `SELECT provider,account,action_class,repository_id,pull_number FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&provider, &account, &class, &repository, &pull)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict()
		}
		if err != nil {
			return err
		}
		if provider != a.Provider || account != a.Account || class != "hosted" || repository != a.Anchor.RepositoryID || pull != a.Anchor.PullNumber {
			return conflict()
		}
		// This event, the ordinary failed transition and exact reservation release
		// share one transaction. No findings, comparisons or round counts change.
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.hosted_incomplete_released", a.ID, a.Revision+1, struct {
			OperationID     string `json:"operation_id"`
			ExternalID      string `json:"external_id"`
			CaptureID       string `json:"capture_id"`
			CaptureSHA256   string `json:"capture_sha256"`
			FirstObservedAt string `json:"first_observed_at"`
			ReleasedAt      string `json:"released_at"`
		}{o.ID, a.ExternalID, source.CaptureID, source.SHA256, firstObserved, now.Format(time.RFC3339Nano)})
	})
}
