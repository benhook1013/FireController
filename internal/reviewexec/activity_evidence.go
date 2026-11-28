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
	"github.com/benhook1013/FireController/internal/reviewprovider"
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
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(e.body_json AS BLOB))<=2048 THEN e.body_json ELSE NULL END,a.id,o.head_sha,a.external_id FROM review_operations o JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id JOIN review_activity_evidence e ON e.attempt_id=a.id AND e.project_id=o.project_id WHERE o.project_id=? AND o.pull_number=? AND o.channel=? AND o.state IN ('active','uncertain') AND a.state=o.state ORDER BY o.created_at DESC,o.id DESC LIMIT 1`, project, number, channel).Scan(&body, &attemptID, &head, &external)
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

// SubmissionEvidence dates a verified posted request, not provider execution.
type SubmissionEvidence struct {
	AttemptID   string `json:"attempt_id"`
	HeadSHA     string `json:"head_sha"`
	TriggerID   string `json:"trigger_id"`
	SubmittedAt string `json:"submitted_at"`
}

func validSubmissionEvidence(v SubmissionEvidence) bool {
	id, err := strconv.ParseInt(v.TriggerID, 10, 64)
	_, valid := hostedObservationTime(v.SubmittedAt)
	return valid && err == nil && id > 0 && strconv.FormatInt(id, 10) == v.TriggerID &&
		hostedObservationID(v.AttemptID, "review_attempt_") && sha.MatchString(v.HeadSHA)
}

func validHostedSubmissionWindow(submittedAt, observedAt, admittedAt string) bool {
	submitted, submittedOK := hostedObservationTime(submittedAt)
	observed, observedOK := hostedObservationTime(observedAt)
	admitted, admittedOK := hostedObservationTime(admittedAt)
	return submittedOK && observedOK && admittedOK && !submitted.After(observed) &&
		!submitted.Before(admitted.Truncate(time.Second).Add(-reviewprovider.HostedProviderClockSkew))
}

// RecordHostedSubmissionEvidence follows the trusted runtime's exact trigger match.
// Repeated observations can fill an absent receipt without adjusting retained times.
func (e *Engine) RecordHostedSubmissionEvidence(ctx context.Context, project, attemptID string, anchor Anchor, trigger reviewprovider.HostedTriggerRecovery, observed time.Time) error {
	v := SubmissionEvidence{AttemptID: attemptID, HeadSHA: anchor.CandidateSHA, TriggerID: strconv.FormatInt(trigger.CommentID, 10), SubmittedAt: trigger.CreatedAt.Format(time.RFC3339Nano)}
	observedAt := observed.Format(time.RFC3339Nano)
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || trigger.State != "adopted" || !validEvidenceAnchor(anchor) ||
		!validSubmissionEvidence(v) || observed.After(time.Now().UTC().Add(5*time.Second)) {
		return invalid()
	}
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, o, err := readHostedPostingAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		if *a.Anchor != anchor || a.ExternalID != v.TriggerID || (a.State != "active" && a.State != "uncertain") {
			return conflict()
		}
		admitted, err := readHostedAdmissionTime(ctx, tx, a, o)
		if err != nil {
			return err
		}
		admittedAt := admitted.Format(time.RFC3339Nano)
		if !validHostedSubmissionWindow(v.SubmittedAt, observedAt, admittedAt) {
			return invalid()
		}
		var retainedProject, retainedHead, retainedTrigger, retainedTime, retainedObserved string
		err = tx.QueryRowContext(ctx, `SELECT project_id,head_sha,trigger_id,submitted_at,observed_at FROM review_hosted_submission_evidence WHERE attempt_id=?`, a.ID).
			Scan(&retainedProject, &retainedHead, &retainedTrigger, &retainedTime, &retainedObserved)
		if err == nil {
			if retainedProject != project || retainedHead != v.HeadSHA || retainedTrigger != v.TriggerID ||
				retainedTime != v.SubmittedAt || !validHostedSubmissionWindow(retainedTime, retainedObserved, admittedAt) {
				return conflict()
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_submission_evidence(attempt_id,project_id,head_sha,trigger_id,submitted_at,observed_at) VALUES(?,?,?,?,?,?)`,
			a.ID, project, v.HeadSHA, v.TriggerID, v.SubmittedAt, observedAt); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.activity_observed", a.ID, 1, struct{}{})
	})
}

// ValidateHostedSubmissionEvidenceSnapshot checks restored diagnostic custody.
func ValidateHostedSubmissionEvidenceSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT s.attempt_id,s.head_sha,s.trigger_id,s.submitted_at,s.observed_at,
 s.project_id,a.project_id,o.project_id,o.attempt_id,o.head_sha,o.channel,a.external_id,coalesce(t.created_at,'')
 FROM review_hosted_submission_evidence s JOIN review_execution_attempts a ON a.id=s.attempt_id
 JOIN review_operations o ON o.id=a.operation_id
 LEFT JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=2 AND t.state='posting' AND t.reason='admitted'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v SubmissionEvidence
		var observed, project, attemptProject, operationProject, operationAttempt, head, channel, trigger, admitted string
		if err := rows.Scan(&v.AttemptID, &v.HeadSHA, &v.TriggerID, &v.SubmittedAt, &observed,
			&project, &attemptProject, &operationProject, &operationAttempt, &head, &channel, &trigger, &admitted); err != nil {
			return err
		}
		if !validSubmissionEvidence(v) || project != attemptProject || project != operationProject ||
			v.AttemptID != operationAttempt || v.HeadSHA != head || v.TriggerID != trigger ||
			channel != "hosted" || !validHostedSubmissionWindow(v.SubmittedAt, observed, admitted) {
			return hostedObservationUnavailable()
		}
	}
	return rows.Err()
}

func readSubmissionEvidence(ctx context.Context, tx *sql.Tx, project string, number int64, channel string) (*SubmissionEvidence, error) {
	if channel != "hosted" {
		return nil, nil
	}
	var v SubmissionEvidence
	var submitted, observed, admitted, acknowledgement sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.id,o.head_sha,a.external_id,s.submitted_at,s.observed_at,admission.created_at,t.created_at
 FROM review_operations o JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id
 LEFT JOIN review_hosted_submission_evidence s ON s.attempt_id=a.id AND s.project_id=o.project_id AND s.head_sha=o.head_sha AND s.trigger_id=a.external_id
 LEFT JOIN review_execution_transitions admission ON s.attempt_id IS NOT NULL AND admission.attempt_id=a.id AND admission.revision=2 AND admission.state='posting' AND admission.reason='admitted'
 LEFT JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.state='active' AND t.reason='trigger_recorded' AND t.external_id=a.external_id
 WHERE o.project_id=? AND o.pull_number=? AND o.channel='hosted' AND a.state=o.state
 AND (o.state IN ('active','uncertain') OR (o.state='failed' AND o.reason=?))
 AND (s.attempt_id IS NOT NULL OR t.attempt_id IS NOT NULL)
 ORDER BY o.created_at DESC,o.id DESC,t.revision ASC LIMIT 1`, project, number, HostedFinishedIncompleteReason).
		Scan(&v.AttemptID, &v.HeadSHA, &v.TriggerID, &submitted, &observed, &admitted, &acknowledgement)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.SubmittedAt = acknowledgement.String
	if validHostedSubmissionWindow(submitted.String, observed.String, admitted.String) {
		v.SubmittedAt = submitted.String
	}
	if !validSubmissionEvidence(v) {
		return nil, nil
	}
	return &v, nil
}
