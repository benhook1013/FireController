package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const operationWaitHeader = "X-FC-Operation-Wait"

// Version 1 advertised Hosted cooldown baselines only. Keep its existing CLI
// queued/pause shape; version 2 additionally understands CLI quota baselines.
// Version 3 adds the retained provider allowance estimate, never a retry clock.
// Version 4 extends version 2 with source-retry evidence; it omits quota estimates.
// Version 5 extends version 3 with accepted Hosted queue priority, not source retry.
func operationWaitForResponse(wait *OperationWait, channel, version string) *OperationWait {
	if wait == nil || version != "1" && version != "2" && version != "3" && version != "4" && version != "5" {
		return nil
	}
	copy := *wait
	if version != "5" {
		copy.QueueAheadPullNumber = 0
	}
	if version != "4" {
		copy.SourceRetry = nil
	}
	wait = &copy
	if version == "1" && channel == "cli" && wait.BaselineAttemptID != "" {
		return &OperationWait{Paused: wait.Paused}
	}
	if version != "3" && version != "5" {
		copy := *wait
		copy.CLIAllowance = nil
		return &copy
	}
	return wait
}

// OperationWait retains dispatch scheduling, not provider availability or review
// completion. A paused ordinary queued operation need not have a cooldown.
type OperationWait struct {
	QueueAheadPullNumber int64                 `json:"queue_ahead_pull_number,omitempty"`
	SourceRetry          *SourceRetry          `json:"source_retry,omitempty"`
	CLIAllowance         *AllowanceObservation `json:"cli_allowance,omitempty"`
	BaselineAttemptID    string                `json:"baseline_attempt_id,omitempty"`
	RetryNotBefore       string                `json:"retry_not_before,omitempty"`
	Paused               bool                  `json:"paused"`
}

func waitUnavailable() error {
	return newStoredUnavailable(storedWaitingIntent, "Stored review waiting intent is unavailable.")
}

func isWaitUnavailable(err error) bool {
	return isStoredUnavailable(err, storedWaitingIntent)
}

// Only absent or proven malformed retained dependencies describe a broken wait.
// Operational storage failures, including already normalized ones, keep their cause.
func waitDependencyError(err error) error {
	if errors.Is(err, sql.ErrNoRows) || storedAllowanceUnavailable(err) || isStoredUnavailable(err, storedHostedObservation) || isStoredUnavailable(err, storedReviewAdmission) {
		return waitUnavailable()
	}
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "not_found" {
		return waitUnavailable()
	}
	return err
}

func readOperationWait(ctx context.Context, q EvidenceQuery, o Operation) (*OperationWait, error) {
	var version string
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil {
		return nil, err
	}
	if version != "9" && version != "10" {
		return nil, nil
	}
	var result OperationWait
	var baseline sql.NullString
	var paused int64
	err := q.QueryRowContext(ctx, `SELECT baseline_attempt_id,retry_not_before,paused FROM review_operation_waits WHERE operation_id=?`, o.ID).Scan(&baseline, &result.RetryNotBefore, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		source, sourceErr := readSourceRetry(ctx, q, o)
		if sourceErr != nil || source == nil {
			return nil, sourceErr
		}
		return &OperationWait{SourceRetry: source}, nil
	}
	if err != nil {
		return nil, err
	}
	result.BaselineAttemptID, result.Paused = baseline.String, paused == 1
	if paused != 0 && paused != 1 || baseline.Valid != (result.RetryNotBefore != "") || baseline.Valid && (!hostedObservationID(baseline.String, "review_attempt_") || !validChannel(o.Channel)) {
		return nil, waitUnavailable()
	}
	if baseline.Valid {
		if _, ok := hostedObservationTime(result.RetryNotBefore); !ok {
			return nil, waitUnavailable()
		}
		a, err := readAttempt(ctx, q, o.ProjectID, baseline.String)
		if err != nil {
			return nil, waitDependencyError(err)
		}
		if a.State != "held" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
			return nil, waitUnavailable()
		}
		original, err := historicalCooldownOperation(ctx, q, o, a)
		if err != nil {
			return nil, err
		}
		if o.Channel == "cli" {
			record, err := readCLIAllowanceRecord(ctx, q, original, a)
			if err != nil {
				return nil, waitDependencyError(err)
			}
			if record.Observation.Remaining != 0 {
				return nil, waitUnavailable()
			}
			in := record.Observation
			result.CLIAllowance = &AllowanceObservation{Kind: "cli_exhausted", ObservedAt: in.ObservedAt, Remaining: &in.Remaining, Limit: &in.Limit, FullCapacityInMS: &in.FullCapacityInMS, RollingWindowMS: &in.RollingWindowMS}
		} else {
			record, err := readHostedAllowanceRecord(ctx, q, original, a)
			if err != nil {
				return nil, waitDependencyError(err)
			}
			if record.Input.Observation.State != "cooldown_observed" {
				return nil, waitUnavailable()
			}
		}
		latest, err := latestCooldownAttempt(ctx, q, o)
		if err != nil {
			return nil, err
		}
		if latest.Provider != a.Provider || latest.Account != a.Account {
			return nil, waitUnavailable()
		}
		due, err := retainedCooldownDeadline(ctx, q, o, latest)
		if err != nil {
			return nil, err
		}
		if due.UTC().Format(time.RFC3339Nano) != result.RetryNotBefore {
			return nil, waitUnavailable()
		}
	}
	result.SourceRetry, err = readSourceRetry(ctx, q, o)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// currentOperationWait exposes pending scheduling only; retained rows still
// bind historical cooldowns after a later attempt starts or the request ends.
func currentOperationWait(o Operation) *OperationWait {
	if o.State != "queued" || o.AttemptID != "" {
		return nil
	}
	return o.Wait
}

func (e *Engine) sampleExecutionTime() time.Time {
	if e.schedulingNow != nil {
		return e.schedulingNow()
	}
	return time.Now()
}

func mustWaitTime(value string) time.Time {
	// readOperationWait already validated the canonical timestamp.
	at, _ := time.Parse(time.RFC3339Nano, value)
	return at
}

// historicalCooldownOperation supplies immutable original attribution for local
// history reads only. Live query/admission/action checks still require the real
// operation's current attempt. Only proven unused cooldown attempts qualify.
func historicalCooldownOperation(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (Operation, error) {
	if o.AttemptID == a.ID {
		return o, nil
	}
	if o.Channel == "cli" {
		return historicalCLIExhaustionOperation(ctx, q, o, a)
	}
	if o.Channel != "hosted" || a.OperationID != o.ID || a.ProjectID != o.ProjectID || a.State != "held" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
		return Operation{}, waitUnavailable()
	}
	var reason, state string
	var owns bool
	if err := q.QueryRowContext(ctx, `SELECT state,reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&state, &reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, waitUnavailable()
		}
		return Operation{}, err
	}
	if state != "held" || reason != "hosted_allowance_cooldown" {
		return Operation{}, waitUnavailable()
	}
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operation_waits WHERE operation_id=? AND baseline_attempt_id IS NOT NULL)`, o.ID).Scan(&owns); err != nil {
		return Operation{}, err
	}
	if !owns {
		return Operation{}, waitUnavailable()
	}
	o.AttemptID, o.State, o.Reason = a.ID, "held", reason
	o.Wait = nil
	return o, nil
}

// RetryBaseline returns the first proven cooldown's source, never permission
// to reuse its allowance query. Every fresh attempt must compare before posting
// its own query and must independently pass the normal final action guards.
func (e *Engine) RetryBaseline(ctx context.Context, project, operationID string) (*Anchor, error) {
	if !identifier.MatchString(project) || !hostedObservationID(operationID, "review_operation_") {
		return nil, invalid()
	}
	var result *Anchor
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperationBase(ctx, tx, project, operationID)
		if err != nil || o.Wait == nil || o.Wait.BaselineAttemptID == "" {
			return err
		}
		a, err := readAttempt(ctx, tx, project, o.Wait.BaselineAttemptID)
		if err != nil {
			return err
		}
		original, err := historicalCooldownOperation(ctx, tx, o, a)
		if err != nil {
			return err
		}
		if o.Channel == "cli" {
			record, err := readCLIAllowanceRecord(ctx, tx, original, a)
			if err == nil {
				anchor := record.Observation.Anchor
				result = &anchor
			}
			return err
		}
		record, err := readHostedAllowanceRecord(ctx, tx, original, a)
		if err == nil {
			anchor := record.Input.Anchor
			result = &anchor
		}
		return err
	})
	return result, err
}

// ScheduleHostedCooldown atomically ends a proven unused attempt and releases
// shared capacity while preserving the same queued authorization. It never
// reopens a terminal request and never accepts caller-supplied cooldown facts.
func (e *Engine) ScheduleHostedCooldown(ctx context.Context, project, attemptID string, expectedAttemptRevision int64) (Attempt, error) {
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		result, err = e.scheduleHostedCooldown(ctx, tx, project, attemptID, expectedAttemptRevision)
		return err
	})
	if err == nil {
		e.notifyOperation(result.OperationID)
	}
	return result, err
}

// Used by normal preparation and startup recovery in their existing atomic write.
func (e *Engine) scheduleHostedCooldown(ctx context.Context, tx *sql.Tx, project, attemptID string, expectedAttemptRevision int64) (Attempt, error) {
	a, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	o, err := readOperationBase(ctx, tx, project, a.OperationID)
	if err != nil {
		return Attempt{}, err
	}
	if a.Revision != expectedAttemptRevision || a.State != "preparing" || o.State != "preparing" || o.AttemptID != a.ID || o.Channel != "hosted" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || a.Revision == math.MaxInt64 || o.Revision == math.MaxInt64 {
		return Attempt{}, conflict()
	}
	record, err := readHostedAllowanceRecord(ctx, tx, o, a)
	if err != nil {
		return Attempt{}, err
	}
	if record.Input.Observation.State != "cooldown_observed" {
		return Attempt{}, waitUnavailable()
	}
	now := e.sampleExecutionTime().UTC()
	due := record.Input.Observation.RetryNotBefore
	if !due.After(now) || o.Wait != nil && o.Wait.RetryNotBefore != "" && !due.After(mustWaitTime(o.Wait.RetryNotBefore)) {
		return Attempt{}, core.Fail("review_cooldown_elapsed", "The proven cooldown deadline has already elapsed; retain an explicit hold instead of retrying immediately.", 409)
	}
	at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
	if err != nil {
		return Attempt{}, err
	}
	recorded, valid := attemptRecordedTime(at)
	if !valid || !due.After(recorded) {
		return Attempt{}, core.Fail("review_cooldown_elapsed", "The proven cooldown deadline has already elapsed; retain an explicit hold instead of retrying immediately.", 409)
	}
	baseline := a.ID
	if o.Wait != nil && o.Wait.BaselineAttemptID != "" {
		baseline = o.Wait.BaselineAttemptID
	}
	a.State, a.Revision = "held", a.Revision+1
	if _, err := tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='held',revision=? WHERE id=?`, a.Revision, a.ID); err != nil {
		return Attempt{}, err
	}
	// Project the real terminal preparation attempt while it is still the
	// operation's current attempt, then detach it for the durable wait.
	if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason='hosted_allowance_cooldown',revision=revision+1,updated_at=? WHERE id=?`, at, o.ID); err != nil {
		return Attempt{}, err
	}
	if err := e.transitionRow(ctx, tx, a, "hosted_allowance_cooldown", at); err != nil {
		return Attempt{}, err
	}
	// Projection reads the previous detached cooldown while this attempt is
	// still current. Install its replacement timer before atomically detaching.
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,?,?,0) ON CONFLICT(operation_id) DO UPDATE SET baseline_attempt_id=excluded.baseline_attempt_id,retry_not_before=excluded.retry_not_before`, o.ID, baseline, due.Format(time.RFC3339Nano)); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason='',attempt_id='' WHERE id=?`, o.ID); err != nil {
		return Attempt{}, err
	}
	if err := core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
		return Attempt{}, err
	}
	return a, nil
}

type OperationControl struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Action           string `json:"action"`
}

type OperationControlReceipt struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Revision       int64  `json:"revision"`
	Paused         bool   `json:"paused"`
	RetryNotBefore string `json:"retry_not_before,omitempty"`
}

func (e *Engine) ControlOperation(ctx context.Context, scope core.Scope, key, id string, input OperationControl) (json.RawMessage, error) {
	if !hostedObservationID(id, "review_operation_") || input.ExpectedRevision < 1 || input.Action != "pause" && input.Action != "resume" && input.Action != "cancel" {
		return nil, invalid()
	}
	result, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.operation.control", Target: id, Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		o, err := readOperationBase(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if o.Revision != input.ExpectedRevision || o.Revision == math.MaxInt64 {
			return nil, conflict()
		}
		if o.State != "queued" || o.AttemptID != "" {
			return nil, core.Fail("review_operation_started", "Only an undispatched queued review can be paused, resumed or cancelled; provider work is unchanged.", 409)
		}
		paused := input.Action == "pause"
		state := "queued"
		if input.Action == "cancel" {
			state = "cancelled"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_operation_waits(operation_id,paused) VALUES(?,?) ON CONFLICT(operation_id) DO UPDATE SET paused=excluded.paused`, id, paused); err != nil {
			return nil, err
		}
		at, err := attemptRecordingStamp(ctx, tx, o, nil, e.sampleAttemptTime())
		if err != nil {
			return nil, err
		}
		reason := ""
		if state == "queued" && (o.Reason == cliDraftWait || o.Reason == sourceRetryReason) {
			reason = o.Reason
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,reason=?,updated_at=? WHERE id=?`, state, reason, at, id); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "review.operation_updated", id, o.Revision+1, map[string]string{"action": input.Action}); err != nil {
			return nil, err
		}
		receipt := OperationControlReceipt{ID: id, State: state, Revision: o.Revision + 1, Paused: paused}
		if o.Wait != nil {
			receipt.RetryNotBefore = o.Wait.RetryNotBefore
		}
		return receipt, nil
	})
	if err == nil {
		e.notifyOperation(id)
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (e *Engine) controlOperationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var input OperationControl
	if err := core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ControlOperation(r.Context(), scope, key, r.PathValue("operation_id"), input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
