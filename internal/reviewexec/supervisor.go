package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// Preparation is the adapter's verified exact target and action body. Preparing
// may read providers and local Git, but must not post or spend a review. Hosted
// preparation may issue its separately journaled, fixed non-review allowance
// query; neither that query nor its reply grants final dispatch permission.
type Preparation struct {
	Anchor Anchor
	Intent string
}

// Result describes external execution only. Completed is not policy credit or
// merge readiness. The adapter must retain its bounded capture/evidence before
// returning a proven terminal result; unresolved outcomes return uncertain.
type Result struct {
	State, Reason, ExternalID string
}

// Driver is an in-process operator-selected adapter, never a client-provided
// command. The journal records intent before Dispatch can perform its action.
type Driver interface {
	Prepare(context.Context, Operation, Configuration, Attempt) (Preparation, error)
	Dispatch(context.Context, Operation, Attempt, func(context.Context, string) error) (Result, error)
}

// Start validates and owns one supervisor, committing crash recovery before
// returning success. The owner calls it before accepting requests, then waits
// for the single terminal result and closed channel during shutdown.
func (e *Engine) Start(ctx context.Context, drivers map[string]Driver) (<-chan error, error) {
	if ctx == nil {
		return nil, invalid()
	}
	owned := make(map[string]Driver, len(drivers))
	for id, d := range drivers {
		if d == nil || reflect.ValueOf(d).Kind() == reflect.Pointer && reflect.ValueOf(d).IsNil() {
			return nil, invalid()
		}
		if _, ok := e.capabilities[id]; !ok {
			return nil, invalid()
		}
		owned[id] = d
	}
	for id := range e.capabilities {
		if owned[id] == nil {
			return nil, invalid()
		}
	}
	if !e.running.CompareAndSwap(false, true) {
		return nil, conflict()
	}
	if err := e.RecoverInterrupted(ctx); err != nil {
		e.running.Store(false)
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := e.supervise(ctx, owned)
		e.running.Store(false)
		done <- err
		close(done)
	}()
	return done, nil
}

// supervise owns the same two finite work slots; provider waits never hold a
// database transaction. Start has already completed startup recovery.
func (e *Engine) supervise(ctx context.Context, owned map[string]Driver) error {
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	failures := make(chan error, 1)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			timer := time.NewTimer(0)
			defer timer.Stop()
			for {
				select {
				case <-workCtx.Done():
					return
				case <-e.wake:
				case <-timer.C:
				}
				if workCtx.Err() != nil {
					return
				}
				if err := e.processNext(workCtx, owned); err != nil {
					select {
					case failures <- err:
					default:
					}
					cancelWork()
					return
				}
				timer.Reset(500 * time.Millisecond)
			}
		})
	}
	workers.Wait()
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}

func (e *Engine) pending(ctx context.Context) ([]Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Snapshot candidates and reservations together, without entering writer
	// admission. StartAttempt still rechecks reservations transactionally.
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	args := make([]any, 0, 4*len(e.capabilities))
	for _, capability := range e.capabilities {
		args = append(args, capability.ID, capability.Provider, capability.Account, capability.Fingerprint)
	}
	args = append(args, e.sampleExecutionTime().UTC().Format(time.RFC3339Nano))
	// Only matching retained bindings can be skipped as busy. Missing/changed
	// capabilities remain candidates for StartAttempt's normal hold handling.
	query := `WITH installed(id,provider,account,fingerprint) AS (VALUES (NULL,NULL,NULL,NULL)` + strings.Repeat(`,(?,?,?,?)`, len(e.capabilities)) + `)
	SELECT o.project_id,o.id FROM review_operations o
	JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
	JOIN projects p ON p.id=o.project_id
	LEFT JOIN installed i ON i.id=CASE WHEN json_valid(c.body_json) THEN json_extract(c.body_json,'$.capability_id') END AND i.fingerprint=o.capability_fingerprint
	WHERE o.state='queued'
AND NOT EXISTS (SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND (w.paused=1 OR (w.retry_not_before<>'' AND julianday(w.retry_not_before)>julianday(?))))
AND NOT EXISTS (SELECT 1 FROM review_execution_config latest WHERE latest.project_id=o.project_id AND latest.revision=(SELECT MAX(revision) FROM review_execution_config WHERE project_id=o.project_id) AND json_valid(latest.body_json) AND json_extract(latest.body_json,'$.paused')=1)
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.account=i.account AND r.action_class=o.channel)
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.repository_id=p.github_repository_id AND r.pull_number=o.pull_number AND r.action_class=o.channel)
	ORDER BY o.created_at,o.id LIMIT 256`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	type id struct{ project, operation string }
	var ids []id
	for rows.Next() {
		var i id
		if err = rows.Scan(&i.project, &i.operation); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, i)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(ids))
	for _, i := range ids {
		o, err := readOperation(ctx, tx, i.project, i.operation)
		if err != nil {
			return nil, err
		}
		if o.State == "queued" {
			if o.Channel == "hosted" {
				// Native admission caps all pending work at 256 (32 per
				// project), so this within-project filter sees the entire
				// eligible candidate set under the SQL limit above.
				c, revision, err := readConfiguration(ctx, tx, o.ProjectID)
				if err != nil {
					return nil, err
				}
				earlier, err := e.earlierQueuedHostedIntent(ctx, tx, o, c, revision)
				if err != nil {
					return nil, err
				}
				if earlier != nil {
					continue
				}
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// preparationReason exposes only selected stable classifications, never adapter
// diagnostics, paths, provider response text or arbitrary error details.
func preparationReason(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) {
		if failure.Code == "review_evidence_pending" || failure.Code == "review_capacity" || roundControlRefusal(failure.Code) {
			return failure.Code
		}
		if details, ok := failure.Details.(map[string]string); failure.Code == "review_held" && ok {
			switch reason := details["reason"]; reason {
			case "provider_activity_rate_limited", "provider_activity_unauthorized", "provider_activity_not_found", "provider_activity_response_invalid", "provider_activity_unavailable", "provider_activity_ambiguous", "provider_activity_state_unknown", "provider_activity_observation_invalid":
				return reason
			case "source_obligations_pending", "repository_changed", "allowance_exhausted", "allowance_unavailable", "allowance_command_not_started", "allowance_timeout", "allowance_command_failed", "allowance_response_unproven", "target_already_covered", "predecessor_identity_changed", "predecessor_unavailable", "predecessor_unfinished", "predecessor_parent_moved", "execution_binding_changed", "preparation_duplicate", "source_changed", "target_changed", "parent_moved", "parent_unavailable", "parent_identity_changed", "default_base_changed", "source_unavailable", "source_fetch_timeout", "workspace_unavailable", "workspace_changed", "comparison_unavailable", "project_file_limit", "provider_activity_unproven", "provider_review_active", "provider_actor_unproven", "hosted_allowance_unproven", "hosted_allowance_reply_pending", "hosted_allowance_cooldown", "standing_baseline_unproven":
				return reason
			}
		}
	}
	return "preparation_failed"
}

func (e *Engine) processNext(ctx context.Context, drivers map[string]Driver) error {
	if !e.beginDispatchWork() {
		return nil
	}
	defer e.endDispatchWork()
	operations, err := e.pending(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return persistenceFailure()
	}
	for _, o := range operations {
		if ctx.Err() != nil {
			return nil
		}
		a, err := e.StartAttempt(ctx, o.ProjectID, o.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var failure *core.Error
			if errors.As(err, &failure) {
				switch failure.Code {
				case "review_busy", "review_conflict", "overloaded":
					continue
				}
			}
			return persistenceFailure()
		}
		if a.ID == "" {
			continue
		}
		// The operation retained its exact configuration revision; StartAttempt
		// has checked that any intervening queue changes are strictly after this target.
		c, err := e.configurationRevision(ctx, o.ProjectID, o.ConfigurationRevision)
		if err != nil {
			if ctx.Err() != nil {
				return e.finishPreparation(a, "cancelled", "supervisor_stopping")
			}
			return persistenceFailure()
		}
		driver := drivers[c.CapabilityID]
		if driver == nil {
			return e.finishPreparation(a, "held", "capability_unavailable")
		}
		// Cold source retrieval has its own sixty-second bound. Leave room for
		// source verification, workspace materialization and allowance checks.
		// Result collection remains independently bounded by the caller's wait.
		prepareCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		prepared, err := driver.Prepare(prepareCtx, o, c, a)
		cancel()
		if err != nil {
			state := "held"
			reason := preparationReason(err)
			if ctx.Err() == nil && o.Channel == "hosted" && reason == "hosted_allowance_cooldown" {
				_, scheduleErr := e.ScheduleHostedCooldown(ctx, a.ProjectID, a.ID, a.Revision)
				if scheduleErr == nil {
					return nil
				}
				var refusal *core.Error
				if !errors.As(scheduleErr, &refusal) || refusal.Code != "review_cooldown_elapsed" {
					return persistenceFailure()
				}
			}
			if ctx.Err() != nil {
				state = "cancelled"
				reason = "supervisor_stopping"
			}
			_, finishErr := e.persistAdvance(a, state, reason, "", sourceRefusalFromError(err, state, reason, o.PullNumber))
			return finishErr
		}
		admitted, err := e.Admit(ctx, o.ProjectID, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			if ctx.Err() != nil {
				return e.finishPreparation(a, "cancelled", "supervisor_stopping")
			}
			var refusal *core.Error
			// A protective source hold may arrive during successful preparation.
			// Admit has refused before recording intent or invoking Dispatch, so
			// settle this unused reservation through the normal held transition.
			if errors.As(err, &refusal) && refusal.Code == "review_held" && preparationReason(err) == "source_obligations_pending" {
				return e.finishPreparation(a, "held", "source_obligations_pending")
			}
			if errors.As(err, &refusal) && (refusal.Code == "review_conflict" || refusal.Code == "validation_failed" || roundControlRefusal(refusal.Code)) {
				return e.finishPreparation(a, "held", "admission_changed")
			}
			return persistenceFailure()
		}
		a = admitted
		var progressMu sync.Mutex
		recordTrigger := func(progressCtx context.Context, externalID string) error {
			progressMu.Lock()
			defer progressMu.Unlock()
			if a.State == "active" && a.ExternalID == externalID && externalID != "" {
				return nil
			}
			if a.State != "posting" {
				return conflict()
			}
			if progressCtx.Err() != nil {
				return progressCtx.Err()
			}
			next, err := e.persistAdvance(a, "active", "trigger_recorded", externalID)
			if err == nil {
				a = next
			}
			return err
		}
		dispatchCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		result, err := driver.Dispatch(dispatchCtx, o, a, recordTrigger)
		cancel()
		progressMu.Lock()
		defer progressMu.Unlock()
		if err != nil {
			result = Result{State: "uncertain", Reason: "dispatch_interrupted", ExternalID: result.ExternalID}
		}
		if result.Reason == "" {
			result.Reason = "adapter_result"
		}
		if !identifier.MatchString(result.Reason) || result.ExternalID != "" && !identifier.MatchString(result.ExternalID) || a.ExternalID != "" && result.ExternalID != "" && a.ExternalID != result.ExternalID {
			result = Result{State: "uncertain", Reason: "adapter_result_invalid", ExternalID: a.ExternalID}
		}
		// Hosted posting must retain its returned trigger before completion. A
		// driver cannot erase a possible post with an invalid/empty result.
		if a.State == "posting" && result.ExternalID != "" {
			next, advanceErr := e.persistAdvance(a, "active", "trigger_recorded", result.ExternalID)
			if advanceErr != nil {
				return advanceErr
			}
			a = next
		}
		if !allowedTransition(a.State, result.State) {
			result = Result{State: "uncertain", Reason: "adapter_result_incomplete"}
		}
		_, err = e.persistAdvance(a, result.State, result.Reason, result.ExternalID)
		return err
	}
	return nil
}

func (e *Engine) configurationRevision(ctx context.Context, project string, revision int64) (Configuration, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var c Configuration
	var body string
	err := e.store.DB().QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, project, revision).Scan(&body)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(body), &c); err != nil {
		return c, err
	}
	return c, nil
}

func (e *Engine) finishPreparation(a Attempt, state, reason string) error {
	_, err := e.persistAdvance(a, state, reason, "")
	return err
}

func persistenceFailure() error {
	return core.Fail("review_persistence_failed", "Review execution stopped because an attempt outcome could not be recorded; inspect retained evidence before restarting.", 503)
}

// Retry only the same guarded local journal transition, never provider work.
// Reconcile an ambiguous successful commit before considering another write.
// A finite failure stops the supervisor visibly with reservations retained.
func (e *Engine) persistAdvance(a Attempt, state, reason, externalID string, refusal ...*SourceRefusal) (Attempt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var facts *SourceRefusal
	if len(refusal) > 0 {
		facts = refusal[0]
	}
	for attempt := range 3 {
		next, err := e.advanceWithSourceRefusal(ctx, a.ProjectID, a.ID, a.Revision, state, reason, externalID, facts)
		if err == nil {
			return next, nil
		}
		tx, readErr := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if readErr == nil {
			current, lookupErr := readAttempt(ctx, tx, a.ProjectID, a.ID)
			var recordedReason, recordedRefusal string
			if lookupErr == nil {
				lookupErr = tx.QueryRowContext(ctx, `SELECT reason,source_refusal_json FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, current.Revision).Scan(&recordedReason, &recordedRefusal)
			}
			_ = tx.Rollback()
			wantID := externalID
			if wantID == "" {
				wantID = a.ExternalID
			}
			if lookupErr == nil && current.Revision == a.Revision+1 && current.State == state && current.ExternalID == wantID && recordedReason == reason && recordedRefusal == sourceRefusalJSON(facts) {
				e.notifyOperation(a.OperationID)
				return current, nil
			}
		}
		var failure *core.Error
		if ctx.Err() != nil || !errors.As(err, &failure) || failure.Code != "overloaded" || attempt == 2 {
			return Attempt{}, persistenceFailure()
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attempt{}, persistenceFailure()
		case <-timer.C:
		}
	}
	return Attempt{}, persistenceFailure()
}
