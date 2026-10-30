package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) sampleAttemptTime() time.Time {
	now := time.Now
	if e.attemptNow != nil {
		now = e.attemptNow
	}
	return now()
}
func attemptTimingError() error {
	return core.Fail("storage_unavailable", "Stored review attempt timing is unavailable.", 500)
}
func attemptRecordedTime(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, err == nil && !at.IsZero() && value == at.UTC().Format(time.RFC3339Nano)
}

// Validate retained local timing before choosing a new effective recording
// timestamp. Only the selected operation and indexed first/current transitions
// are read; malformed history is refused rather than silently rewritten.
func attemptRecordingStamp(ctx context.Context, tx *sql.Tx, o Operation, a *Attempt, now time.Time) (string, error) {
	created, createdOK := attemptRecordedTime(o.CreatedAt)
	updated, updatedOK := attemptRecordedTime(o.UpdatedAt)
	if !createdOK || !updatedOK || updated.Before(created) {
		return "", attemptTimingError()
	}
	minimum := updated
	if a != nil {
		var firstRevision int64
		var firstAt, currentAt, currentState string
		err := tx.QueryRowContext(ctx, `SELECT revision,created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision LIMIT 1`, a.ID).Scan(&firstRevision, &firstAt)
		if errors.Is(err, sql.ErrNoRows) {
			return "", attemptTimingError()
		}
		if err != nil {
			return "", err
		}
		err = tx.QueryRowContext(ctx, `SELECT state,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&currentState, &currentAt)
		if errors.Is(err, sql.ErrNoRows) {
			return "", attemptTimingError()
		}
		if err != nil {
			return "", err
		}
		first, firstOK := attemptRecordedTime(firstAt)
		current, currentOK := attemptRecordedTime(currentAt)
		if firstRevision != 1 || currentState != a.State || !firstOK || !currentOK || first.Before(created) || current.Before(first) {
			return "", attemptTimingError()
		}
		if current.After(minimum) {
			minimum = current
		}
	}
	if now.Before(minimum) {
		now = minimum
	}
	return now.UTC().Format(time.RFC3339Nano), nil
}

// StartAttempt reserves both the external target and the provider account.
// It does no provider work; preparing has not authorized an external action.
func (e *Engine) StartAttempt(ctx context.Context, project, id string) (Attempt, error) {
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if o.State != "queued" {
			return conflict()
		}
		if o.Wait != nil && (o.Wait.Paused || o.Wait.RetryNotBefore != "" && e.sampleExecutionTime().Before(mustWaitTime(o.Wait.RetryNotBefore))) {
			return nil
		}
		at, err := attemptRecordingStamp(ctx, tx, o, nil, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		c, revision, err := readConfiguration(ctx, tx, project)
		if err != nil {
			return err
		}
		if c.Paused {
			return nil
		}
		compatible, err := compatibleQueuedConfiguration(ctx, tx, o, c, revision)
		if err != nil {
			return err
		}
		if !compatible {
			return holdOperation(ctx, tx, o, "configuration_changed", at)
		}
		capability, ok := e.capabilities[c.CapabilityID]
		if !ok || e.validateConfiguration(c) != nil {
			return holdOperation(ctx, tx, o, "capability_unavailable", at)
		}
		if o.CapabilityFingerprint != capability.Fingerprint {
			return holdOperation(ctx, tx, o, "capability_changed", at)
		}
		allowed, err := actorAllowed(ctx, tx, project, o.ActorID, o.WorkerID)
		if err != nil {
			return err
		}
		if !allowed {
			return holdOperation(ctx, tx, o, "authorization_changed", at)
		}
		_, reason, err := boundPolicy(ctx, tx, o)
		if err != nil {
			return err
		}
		if reason != "" {
			return holdOperation(ctx, tx, o, reason, at)
		}
		if _, err := admitRoundControl(ctx, tx, o); err != nil {
			var refusal *core.Error
			if errors.As(err, &refusal) && (refusal.Code == "review_held" || roundControlRefusal(refusal.Code)) {
				return holdOperation(ctx, tx, o, preparationReason(err), at)
			}
			return err
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if e.publishAttempt != nil {
			if err := validateAttemptProjection(ctx, tx, o); err != nil {
				return err
			}
		}
		var busy int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations WHERE provider=? AND action_class=? AND (account=? OR (repository_id=? AND pull_number=?))`, capability.Provider, o.Channel, capability.Account, repositoryID, o.PullNumber).Scan(&busy); err != nil {
			return err
		}
		if busy != 0 {
			return core.Fail("review_busy", "A review reservation is already occupied; no external action was started.", 409)
		}

		result = Attempt{ID: core.NewID("review_attempt"), OperationID: o.ID, ProjectID: project, Provider: capability.Provider, Account: capability.Account, State: "preparing", Revision: 1}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) VALUES(?,?,?,?,?,'preparing',1,'null','','')`, result.ID, o.ID, project, result.Provider, result.Account); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES(?,?,?,?,?,?)`, result.Provider, repositoryID, o.PullNumber, o.Channel, result.Account, result.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='preparing',attempt_id=?,revision=revision+1,updated_at=? WHERE id=?`, result.ID, at, o.ID); err != nil {
			return err
		}
		return e.transitionRow(ctx, tx, result, "preparing", at)
	})
	if err == nil {
		e.notifyOperation(id)
	}
	return result, err
}

func holdOperation(ctx context.Context, tx *sql.Tx, o Operation, reason, at string) error {
	_, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason=?,revision=revision+1,updated_at=? WHERE id=?`, reason, at, o.ID)
	if err != nil {
		return err
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{})
}

func readAttempt(ctx context.Context, tx EvidenceQuery, project, id string) (Attempt, error) {
	var a Attempt
	var anchor string
	err := tx.QueryRowContext(ctx, `SELECT id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE project_id=? AND id=?`, project, id).Scan(&a.ID, &a.OperationID, &a.ProjectID, &a.Provider, &a.Account, &a.State, &a.Revision, &anchor, &a.Intent, &a.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, core.Fail("not_found", "The review attempt is unavailable in this project.", 404)
	}
	if err == nil && json.Unmarshal([]byte(anchor), &a.Anchor) != nil {
		return a, core.Fail("storage_unavailable", "Stored review admission is unavailable.", 500)
	}
	return a, err
}

// Admit persists the exact comparison and external intent before the adapter
// may post or start its command. The caller owns fresh queue/allowance proof.
func (e *Engine) Admit(ctx context.Context, project, id string, expectedRevision int64, anchor Anchor, intent string) (Attempt, error) {
	observed, err := time.Parse(time.RFC3339Nano, anchor.ObservedAt)
	if err != nil || time.Since(observed) > time.Minute || observed.After(time.Now().Add(5*time.Second)) || anchor.RepositoryID <= 0 || anchor.ParentPullNumber < 0 || anchor.ParentPullNumber > 2147483647 || !safeText(anchor.BaseRef, 256) || anchor.BaseRef == "" || !repository.MatchString(anchor.Repository) || !sha.MatchString(anchor.CandidateSHA) || !sha.MatchString(anchor.BaseSHA) || !sha.MatchString(anchor.ParentSHA) || !sha.MatchString(anchor.MergeBaseSHA) || !hash.MatchString(anchor.PatchSHA256) || !safeText(intent, 4096) {
		return Attempt{}, invalid()
	}
	var result Attempt
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if a.Revision != expectedRevision || a.State != "preparing" {
			return conflict()
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		if anchor.PullNumber != o.PullNumber || anchor.CandidateSHA != o.HeadSHA {
			return conflict()
		}
		if err := requireSourceObligationsClear(ctx, tx, project, o.PullNumber); err != nil {
			return err
		}
		if err := requireIncomingRoutesClear(ctx, tx, project, o.PullNumber); err != nil {
			return err
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != anchor.RepositoryID {
			return conflict()
		}
		c, revision, err := readConfiguration(ctx, tx, project)
		if err != nil {
			return err
		}
		allowed, err := actorAllowed(ctx, tx, project, o.ActorID, o.WorkerID)
		if err != nil {
			return err
		}
		compatible, err := compatibleQueuedConfiguration(ctx, tx, o, c, revision)
		if err != nil {
			return err
		}
		if !allowed || !compatible {
			return conflict()
		}
		capability, ok := e.capabilities[c.CapabilityID]
		if !ok || e.validateConfiguration(c) != nil || capability.Provider != a.Provider || capability.Account != a.Account || capability.Fingerprint != o.CapabilityFingerprint {
			return conflict()
		}
		_, reason, err := boundPolicy(ctx, tx, o)
		if err != nil {
			return err
		}
		if reason != "" {
			return policyAdmissionConflict()
		}
		if _, err := admitRoundControl(ctx, tx, o); err != nil {
			return err
		}
		state := "active"
		if o.Channel == "hosted" {
			state = "posting"
			if intent == "" {
				return invalid()
			}
		} else if intent != "" {
			return invalid()
		}
		at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		a.State = state
		a.Revision++
		a.Anchor = &anchor
		a.Intent = intent
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=?,anchor_json=?,intent=? WHERE id=?`, a.State, a.Revision, encoded(anchor), intent, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,updated_at=? WHERE id=?`, state, at, o.ID); err != nil {
			return err
		}
		if err = e.transitionRow(ctx, tx, a, "admitted", at); err != nil {
			return err
		}
		if state != "posting" && state != "active" && state != "uncertain" {
			if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
				return err
			}
		}
		result = a
		return nil
	})
	if err == nil {
		e.notifyOperation(result.OperationID)
	}
	return result, err
}

func safeText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func (e *Engine) transitionRow(ctx context.Context, tx *sql.Tx, a Attempt, reason, at string, refusal ...*SourceRefusal) error {
	var facts *SourceRefusal
	if len(refusal) > 0 {
		facts = refusal[0]
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at,source_refusal_json) VALUES(?,?,?,?,?,?,?)`, a.ID, a.Revision, a.State, reason, a.ExternalID, at, sourceRefusalJSON(facts))
	if err == nil && e.publishAttempt != nil {
		return e.publishAttempt(ctx, tx, a)
	}
	return err
}

// Advance records one guarded transition. Uncertain attempts retain both
// reservations; no retry, lease expiry or restart silently spends another run.
func (e *Engine) Advance(ctx context.Context, project, id string, expectedRevision int64, state, reason, externalID string) (Attempt, error) {
	return e.advanceWithSourceRefusal(ctx, project, id, expectedRevision, state, reason, externalID, nil)
}

func (e *Engine) advanceWithSourceRefusal(ctx context.Context, project, id string, expectedRevision int64, state, reason, externalID string, refusal *SourceRefusal) (Attempt, error) {
	if !identifier.MatchString(reason) || externalID != "" && !identifier.MatchString(externalID) {
		return Attempt{}, invalid()
	}
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if a.Revision != expectedRevision || !allowedTransition(a.State, state) {
			return conflict()
		}
		if a.ExternalID != "" && externalID != "" && externalID != a.ExternalID {
			return conflict()
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		if !validSourceRefusal(refusal, state, reason, o.PullNumber) {
			return invalid()
		}
		at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		if externalID != "" {
			a.ExternalID = externalID
		}
		if a.State == "posting" && state == "active" && a.ExternalID == "" {
			return invalid()
		}
		a.State = state
		a.Revision++
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=?,external_id=? WHERE id=?`, state, a.Revision, a.ExternalID, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,reason=?,updated_at=? WHERE id=?`, state, reason, at, a.OperationID); err != nil {
			return err
		}
		if state != "posting" && state != "active" && state != "uncertain" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
				return err
			}
		}
		if err = e.transitionRow(ctx, tx, a, reason, at, refusal); err != nil {
			return err
		}
		if state == "completed" {
			if err = refreshComparisonProof(ctx, tx, project, a.OperationID); err != nil {
				return err
			}
			if err = completeRoundControl(ctx, tx, project, a.OperationID); err != nil {
				return err
			}
		}
		if state != "posting" && state != "active" && state != "uncertain" {
			if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
				return err
			}
		}
		result = a
		return nil
	})
	if err == nil {
		e.notifyOperation(result.OperationID)
	}
	return result, err
}

func allowedTransition(from, to string) bool {
	switch from {
	case "preparing":
		return to == "held" || to == "failed" || to == "cancelled" || to == "rate_limited"
	case "posting":
		return to == "active" || to == "uncertain" || to == "failed" || to == "rate_limited"
	case "active":
		return to == "completed" || to == "failed" || to == "cancelled" || to == "rate_limited" || to == "uncertain"
	case "uncertain":
		return to == "active" || to == "completed" || to == "failed" || to == "cancelled" || to == "rate_limited"
	}
	return false
}

// RecoverInterrupted runs once before a new supervisor accepts work. It never
// restarts a possibly dispatched action. Hosted recovery uses its retained
// marker; interrupted CLI work remains uncertain until evidence settles it.
func (e *Engine) RecoverInterrupted(ctx context.Context) error {
	var changed []string
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_execution_attempts WHERE state IN ('preparing','posting','active') ORDER BY id`)
		if err != nil {
			return err
		}
		type identity struct{ project, id string }
		var ids []identity
		for rows.Next() {
			var x identity
			if err = rows.Scan(&x.project, &x.id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, x)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		now := e.sampleAttemptTime()
		for _, x := range ids {
			a, err := readAttempt(ctx, tx, x.project, x.id)
			if err != nil {
				return err
			}
			o, err := readOperation(ctx, tx, x.project, a.OperationID)
			if err != nil {
				return err
			}
			if a.State == "preparing" && o.Channel == "hosted" {
				record, readErr := readHostedAllowanceRecord(ctx, tx, o, a)
				if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
					return readErr
				}
				if readErr == nil && record.Input.Observation.State == "cooldown_observed" && record.Input.Observation.RetryNotBefore.After(e.sampleExecutionTime()) {
					_, scheduleErr := e.scheduleHostedCooldown(ctx, tx, x.project, x.id, a.Revision)
					if scheduleErr == nil {
						changed = append(changed, a.OperationID)
						continue
					}
					var refusal *core.Error
					if !errors.As(scheduleErr, &refusal) || refusal.Code != "review_cooldown_elapsed" {
						return scheduleErr
					}
				}
			}
			at, err := attemptRecordingStamp(ctx, tx, o, &a, now)
			if err != nil {
				return err
			}
			state := "uncertain"
			if a.State == "preparing" {
				state = "cancelled"
			}
			a.State = state
			a.Revision++
			if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=? WHERE id=?`, state, a.Revision, a.ID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,reason='supervisor_interrupted',revision=revision+1,updated_at=? WHERE id=?`, state, at, a.OperationID); err != nil {
				return err
			}
			if state == "cancelled" {
				if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
					return err
				}
			}
			if err = e.transitionRow(ctx, tx, a, "supervisor_interrupted", at); err != nil {
				return err
			}
			if state == "cancelled" {
				if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
					return err
				}
			}
			changed = append(changed, a.OperationID)
		}
		return nil
	})
	if err == nil {
		for _, id := range changed {
			e.notifyOperation(id)
		}
	}
	return err
}
