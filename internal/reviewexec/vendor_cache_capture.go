package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SettledCLICapture is metadata for a separately verified immutable capture.
// It is an observation, not permission to remove vendor files or durable data.
type SettledCLICapture struct {
	AttemptID, ProjectID, OperationID, CaptureID, SHA256 string
	Bytes                                                int64
}

// ObserveSettledCLICapture reuses native journal chronology/retention rules and
// narrows them to completed CLI attempts with matching retained evidence. Callers
// still need account/process custody, exact cache ownership and file verification.
// No public endpoint or cleanup caller is installed by this helper.
func (e *Engine) ObserveSettledCLICapture(ctx context.Context, capabilityID, attemptID string, now time.Time) (SettledCLICapture, error) {
	var zero SettledCLICapture
	ok, err := e.WorkspaceDisposable(ctx, capabilityID, attemptID, now)
	if err != nil || !ok {
		return zero, err
	}
	cap := e.capabilities[capabilityID]
	var project, operation, body, anchor, fingerprint string
	err = e.store.DB().QueryRowContext(ctx, `SELECT a.project_id,a.operation_id,v.body_json,a.anchor_json,o.capability_fingerprint
 FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id AND o.project_id=a.project_id
 JOIN review_execution_evidence v ON v.attempt_id=a.id AND v.operation_id=o.id AND v.project_id=a.project_id
 WHERE a.id=? AND a.provider=? AND a.account=? AND a.state='completed' AND o.state='completed' AND o.channel='cli' AND o.attempt_id=a.id
 AND NOT EXISTS(SELECT 1 FROM review_execution_reservations r WHERE r.attempt_id=a.id)`, attemptID, cap.Provider, cap.Account).Scan(&project, &operation, &body, &anchor, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, nil
	}
	if err != nil {
		return zero, err
	}
	var evidence ReviewEvidence
	var original Anchor
	if len(body) > evidenceInputLimit+8192 || json.Unmarshal([]byte(body), &evidence) != nil || json.Unmarshal([]byte(anchor), &original) != nil || validateReviewEvidence(evidence.Capture) != nil || evidence.AttemptID != attemptID || evidence.ProjectID != project || evidence.OperationID != operation || evidence.CapabilityFingerprint != fingerprint || evidence.Anchor != original || evidence.Capture.ExpectedAnchor != original || evidenceChannel(evidence.Capture) != "cli" || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
		return zero, persistenceFailure()
	}
	ok, err = e.WorkspaceDisposable(ctx, capabilityID, attemptID, now)
	if err != nil || !ok {
		return zero, err
	}
	return SettledCLICapture{attemptID, project, operation, evidence.Capture.CaptureID, evidence.Capture.CaptureSHA256, evidence.Capture.CaptureBytes}, nil
}
