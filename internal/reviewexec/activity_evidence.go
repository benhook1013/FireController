package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// ActivityEvidence is private diagnostic evidence, never review admission,
// completion, current liveness or a Hosted execution start time.
type ActivityEvidence struct {
	AttemptID        string `json:"attempt_id"`
	HeadSHA          string `json:"head_sha"`
	Kind             string `json:"kind"`
	ObservedAt       string `json:"observed_at"`
	ProcessStartedAt string `json:"process_started_at,omitempty"`
	CommentUpdatedAt string `json:"comment_updated_at,omitempty"`
	SourceCommentID  int64  `json:"source_comment_id,omitempty"`
	ProviderRunID    string `json:"provider_run_id,omitempty"`
	TriggerID        string `json:"trigger_id,omitempty"`
}

var activityProviderRun = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func validActivityEvidence(v ActivityEvidence) bool {
	_, ok := hostedObservationTime(v.ObservedAt)
	if !ok || !identifier.MatchString(v.AttemptID) || !sha.MatchString(v.HeadSHA) {
		return false
	}
	switch v.Kind {
	case "cli_process_spawned":
		return v.ProcessStartedAt == v.ObservedAt && v.CommentUpdatedAt == "" && v.SourceCommentID == 0 && v.ProviderRunID == "" && v.TriggerID == ""
	case "hosted_processing_observed":
		updated, valid := hostedObservationTime(v.CommentUpdatedAt)
		observed, _ := hostedObservationTime(v.ObservedAt)
		trigger, triggerErr := strconv.ParseInt(v.TriggerID, 10, 64)
		return valid && triggerErr == nil && trigger > 0 && strconv.FormatInt(trigger, 10) == v.TriggerID && !updated.After(observed) && v.ProcessStartedAt == "" && v.SourceCommentID > 0 && activityProviderRun.MatchString(v.ProviderRunID)
	}
	return false
}

// RecordActivityEvidence retains the first matched signal, idempotently. It is
// called only by the native runtime at a proven spawn or parsed observation.
func (e *Engine) RecordActivityEvidence(ctx context.Context, project, attemptID string, v ActivityEvidence) error {
	if !identifier.MatchString(project) || attemptID != v.AttemptID || !validActivityEvidence(v) {
		return invalid()
	}
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		if o.AttemptID != a.ID || o.HeadSHA != v.HeadSHA || a.Anchor == nil || a.Anchor.CandidateSHA != v.HeadSHA || a.State != "active" || o.Channel == "cli" && v.Kind != "cli_process_spawned" || o.Channel == "hosted" && v.Kind != "hosted_processing_observed" {
			return conflict()
		}
		if o.Channel == "hosted" && a.ExternalID != v.TriggerID {
			return conflict()
		}
		boundaryState := "active"
		if o.Channel == "hosted" {
			boundaryState = "posting"
		}
		var activeAt string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND state=? ORDER BY revision LIMIT 1`, a.ID, boundaryState).Scan(&activeAt); err != nil {
			return err
		}
		active, ok := hostedObservationTime(activeAt)
		observed, _ := hostedObservationTime(v.ObservedAt)
		if !ok || observed.Before(active) || observed.After(time.Now().UTC().Add(5*time.Second)) {
			return invalid()
		}
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO review_activity_evidence(attempt_id,project_id,body_json) VALUES(?,?,?) ON CONFLICT(attempt_id) DO NOTHING`, a.ID, project, string(body))
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.activity_observed", a.ID, 1, struct{}{})
	})
}

func readActivityEvidence(ctx context.Context, tx *sql.Tx, project string, number int64, channel string) (*ActivityEvidence, error) {
	var body, attemptID, head, external string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(e.body_json AS BLOB))<=2048 THEN e.body_json ELSE NULL END,a.id,o.head_sha,a.external_id FROM review_operations o JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id JOIN review_activity_evidence e ON e.attempt_id=a.id AND e.project_id=o.project_id WHERE o.project_id=? AND o.pull_number=? AND o.channel=? AND o.state='active' AND a.state='active' ORDER BY o.created_at DESC,o.id DESC LIMIT 1`, project, number, channel).Scan(&body, &attemptID, &head, &external)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v ActivityEvidence
	if len(body) > 2048 || !decodeActivityEvidence(body, &v) || !validActivityEvidence(v) || v.AttemptID != attemptID || v.HeadSHA != head || channel == "cli" && v.Kind != "cli_process_spawned" || channel == "hosted" && (v.Kind != "hosted_processing_observed" || v.TriggerID != external) {
		return nil, hostedObservationUnavailable()
	}
	return &v, nil
}

// ValidateActivityEvidenceSnapshot verifies optional diagnostic rows on restore.
func ValidateActivityEvidenceSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN length(CAST(e.body_json AS BLOB))<=2048 THEN e.body_json ELSE NULL END,e.attempt_id,e.project_id,a.project_id,o.head_sha,o.channel,a.external_id FROM review_activity_evidence e JOIN review_execution_attempts a ON a.id=e.attempt_id JOIN review_operations o ON o.id=a.operation_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body, id, project, owner, head, channel, external string
		if err := rows.Scan(&body, &id, &project, &owner, &head, &channel, &external); err != nil {
			return err
		}
		var v ActivityEvidence
		if len(body) > 2048 || !decodeActivityEvidence(body, &v) || !validActivityEvidence(v) || v.AttemptID != id || project != owner || v.HeadSHA != head || channel == "cli" && v.Kind != "cli_process_spawned" || channel == "hosted" && (v.Kind != "hosted_processing_observed" || v.TriggerID != external) {
			return hostedObservationUnavailable()
		}
	}
	return rows.Err()
}

func decodeActivityEvidence(body string, v *ActivityEvidence) bool {
	if len(body) > 2048 || json.Unmarshal([]byte(body), v) != nil {
		return false
	}
	canonical, err := json.Marshal(v)
	return err == nil && string(canonical) == body
}
