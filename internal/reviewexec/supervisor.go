package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
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
	skipped, err := e.reconcilePreparingDrivers(ctx, owned)
	if err != nil {
		e.running.Store(false)
		return nil, err
	}
	if err := e.recoverInterrupted(ctx, skipped); err != nil {
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

// supervise owns two finite work slots. Either idle worker can probe a Draft
// wait, with a shared permit limiting that probe to one worker at a time.
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

type pendingCandidateID struct{ project, operation string }

type pendingSchedulingCandidate struct {
	pendingCandidateID
	channel, reason string
}

func (e *Engine) pendingCandidates(ctx context.Context) ([]pendingCandidateID, error) {
	candidates, err := e.pendingSchedulingCandidates(ctx, true)
	if err != nil {
		return nil, err
	}
	ids := make([]pendingCandidateID, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.pendingCandidateID)
	}
	return ids, nil
}

func (e *Engine) pendingSchedulingCandidates(ctx context.Context, probeDrafts bool) ([]pendingSchedulingCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Snapshot candidates and reservations together, without entering writer
	// admission. StartAttempt still rechecks reservations transactionally.
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := e.sampleExecutionTime()
	args := make([]any, 0, 6*len(e.capabilities)+3)
	type accountKey struct{ provider, account string }
	quotaHeld := make(map[accountKey]bool)
	for _, capability := range e.capabilities {
		key := accountKey{capability.Provider, capability.Account}
		held, checked := quotaHeld[key]
		if !checked {
			due, err := cliAccountRetryDeadline(ctx, tx, capability, now)
			if err != nil && !isWaitUnavailable(err) {
				return nil, err
			}
			held = err != nil || now.Before(due)
			quotaHeld[key] = held
		}
		args = append(args, capability.ID, capability.Provider, capability.Account, capability.Fingerprint, e.sourceReadGroups[capability.ID], held)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	args = append(args, stamp, stamp, stamp)
	// Only matching retained bindings can be skipped as busy. Missing/changed
	// capabilities remain candidates for StartAttempt's normal hold handling.
	query := `WITH installed(id,provider,account,fingerprint,source_group,cli_quota_held) AS (VALUES (NULL,NULL,NULL,NULL,NULL,0)` + strings.Repeat(`,(?,?,?,?,?,?)`, len(e.capabilities)) + `),
 source_limited AS MATERIALIZED (SELECT DISTINCT source_group FROM review_source_retries INDEXED BY review_source_retries_due WHERE julianday(retry_not_before)>julianday(?))
	SELECT o.project_id,o.id,o.channel,o.reason FROM review_operations o
	JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
	JOIN projects p ON p.id=o.project_id
	LEFT JOIN installed i ON i.id=CASE WHEN json_valid(c.body_json) THEN json_extract(c.body_json,'$.capability_id') END AND i.fingerprint=o.capability_fingerprint
	WHERE o.state='queued'
AND NOT (o.channel='cli' AND COALESCE(i.cli_quota_held,0)=1)
AND NOT EXISTS (SELECT 1 FROM review_pull_scheduling s WHERE s.project_id=o.project_id AND s.pull_number=o.pull_number AND s.paused=1)
AND NOT EXISTS (SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND (w.paused=1 OR (w.retry_not_before<>'' AND julianday(w.retry_not_before)>julianday(?))))
AND NOT EXISTS (SELECT 1 FROM source_limited WHERE source_group=i.source_group)
AND NOT EXISTS (SELECT 1 FROM review_source_retries sr WHERE sr.operation_id=o.id AND julianday(sr.retry_not_before)>julianday(?))
AND NOT EXISTS (SELECT 1 FROM review_execution_config latest WHERE latest.project_id=o.project_id AND latest.revision=(SELECT MAX(revision) FROM review_execution_config WHERE project_id=o.project_id) AND json_valid(latest.body_json) AND json_extract(latest.body_json,'$.paused')=1)
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.account=i.account AND r.action_class=o.channel AND (o.channel='cli' OR r.repository_id=p.github_repository_id))
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.repository_id=p.github_repository_id AND r.pull_number=o.pull_number AND r.action_class=o.channel)
	`
	var ids []pendingSchedulingCandidate
	read := func(suffix string, parameters []any) error {
		rows, err := tx.QueryContext(ctx, query+suffix, parameters...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate pendingSchedulingCandidate
			if err := rows.Scan(&candidate.project, &candidate.operation, &candidate.channel, &candidate.reason); err != nil {
				return err
			}
			ids = append(ids, candidate)
		}
		return rows.Err()
	}
	if probeDrafts {
		e.draftProbeMu.Lock()
		cursor := e.draftProbeCursor
		e.draftProbeMu.Unlock()
		draftArgs := append(append([]any(nil), args...), cursor, cliDraftProbeCapacity+1)
		if err := read(` AND o.channel='cli' AND o.reason='cli_draft_wait' ORDER BY o.id<=?,o.id LIMIT ?`, draftArgs); err != nil {
			return nil, err
		}
	}
	if err := read(` AND NOT (o.channel='cli' AND o.reason='cli_draft_wait')
		ORDER BY CASE WHEN o.channel='cli' AND lower(p.repository)='benhook1013/firemud' THEN 0 ELSE 1 END,o.created_at,o.id LIMIT 256`, args); err != nil {
		return nil, err
	}
	return ids, nil
}

// Expand only the reached candidate in a fresh short snapshot. Actual claim
// still revalidates its complete authority and reservations transactionally.
func (e *Engine) pendingCandidate(ctx context.Context, id pendingCandidateID) (Operation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback()
	o, err := readOperation(ctx, tx, id.project, id.operation)
	if err != nil {
		return o, false, err
	}
	if o.State != "queued" {
		return Operation{}, false, nil
	}
	if validChannel(o.Channel) {
		c, revision, err := readConfiguration(ctx, tx, o.ProjectID)
		if err != nil {
			return o, false, err
		}
		if ready, err := e.sourceCandidateDue(ctx, tx, o, c); err != nil {
			return o, false, err
		} else if !ready {
			return Operation{}, false, nil
		}
		if preferred, err := e.preferredCLIQueued(ctx, tx, o, c); err != nil {
			return o, false, err
		} else if preferred {
			return Operation{}, false, nil
		}
		earlier, err := e.earlierQueuedReviewIntent(ctx, tx, o, c, revision)
		if err != nil {
			return o, false, err
		}
		if earlier != nil {
			return Operation{}, false, nil
		}
		if o.Channel == "cli" {
			if capability, ok := e.capabilities[c.CapabilityID]; ok && capability.Fingerprint == o.CapabilityFingerprint {
				now := e.sampleExecutionTime()
				due, err := cliAccountRetryDeadline(ctx, tx, capability, now)
				if err != nil {
					return o, false, err
				}
				if now.Before(due) {
					return Operation{}, false, nil
				}
			}
		}
	}
	return o, true, nil
}

// preparationReason exposes only selected stable classifications, never adapter
// diagnostics, paths, provider response text or arbitrary error details.
func preparationReason(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) {
		if failure.Code == "review_evidence_pending" || failure.Code == "external_hosted_pending" || failure.Code == "review_capacity" || failure.Code == "pull_scheduling_paused" || failure.Code == "automation_authorization_changed" || roundControlRefusal(failure.Code) {
			return failure.Code
		}
		if details, ok := failure.Details.(map[string]string); failure.Code == "review_held" && ok {
			if details["reason"] == "target_changed" {
				switch reason := details["stop_reason"]; reason {
				case "target_merged", "target_closed", "target_superseded":
					return reason
				}
			}
			switch reason := details["reason"]; reason {
			case "hosted_allowance_history_invalid", "hosted_allowance_history_unavailable", "hosted_allowance_capture_invalid", "hosted_allowance_capture_unavailable", "hosted_allowance_query_post_unproven", "hosted_allowance_query_recording_pending", "hosted_allowance_capture_recording_pending":
				return reason
			case "target_merged", "target_closed", "target_superseded", "pull_draft", "provider_activity_rate_limited", "provider_activity_unauthorized", "provider_activity_not_found", "provider_activity_response_invalid", "provider_activity_unavailable", "provider_activity_ambiguous", "provider_activity_state_unknown", "provider_activity_observation_invalid":
				return reason
			case "routed_findings_pending", "source_obligations_pending", "repository_changed", "allowance_exhausted", "allowance_unavailable", "allowance_command_not_started", "allowance_timeout", "allowance_command_failed", "allowance_response_unproven", "target_already_covered", "predecessor_identity_changed", "predecessor_unavailable", "predecessor_unfinished", "predecessor_parent_moved", "execution_binding_changed", "preparation_duplicate", "source_changed", "target_changed", "parent_moved", "parent_unavailable", "parent_identity_changed", "default_base_changed", "source_unavailable", "source_fetch_timeout", "workspace_unavailable", "workspace_changed", "comparison_unavailable", "project_file_limit", "provider_activity_unproven", "provider_review_active", "provider_actor_unproven", "hosted_allowance_unproven", "hosted_chat_cooldown", "hosted_allowance_reply_pending", "hosted_allowance_cooldown", "standing_baseline_unproven":
				return reason
			}
		}
	}
	var diagnostic *preparationFailure
	if errors.As(err, &diagnostic) {
		return diagnostic.Error()
	}
	return "preparation_failed"
}

// A malformed retained configuration affects this candidate's authority, not
// the database or other projects. Preserve the request and its queue turn. The
// bounded process-local throttle limits diagnostics, never admission or retries.
func (e *Engine) skipUnavailableConfiguration(ctx context.Context, o Operation, err error) bool {
	if !isStoredUnavailable(err, storedConfigurationHistory) {
		return false
	}
	now := e.sampleExecutionTime()
	e.configurationFailureMu.Lock()
	defer e.configurationFailureMu.Unlock()
	if e.configurationFailureNext[o.ID].After(now) {
		return true
	}
	for id, next := range e.configurationFailureNext {
		if !next.After(now) {
			delete(e.configurationFailureNext, id)
		}
	}
	if len(e.configurationFailureNext) >= 256 {
		return true
	}
	if e.configurationFailureNext == nil {
		e.configurationFailureNext = make(map[string]time.Time)
	}
	e.configurationFailureNext[o.ID] = now.Add(30 * time.Second)
	slog.WarnContext(ctx, "Review candidate configuration history unavailable", "project_id", o.ProjectID, "operation_id", o.ID, "reason", "configuration_history_unavailable")
	return true
}

// Typed wait validation failures refuse only this candidate and its guarded scope. Real
// database and context failures retain their normal supervisor behavior.
func (e *Engine) skipUnavailableWait(ctx context.Context, o Operation, err error) bool {
	if !validChannel(o.Channel) || !isWaitUnavailable(err) {
		return false
	}
	now := e.sampleExecutionTime()
	e.configurationFailureMu.Lock()
	defer e.configurationFailureMu.Unlock()
	key := o.Channel + "-wait:" + o.ID
	if e.configurationFailureNext[key].After(now) {
		return true
	}
	for id, next := range e.configurationFailureNext {
		if !next.After(now) {
			delete(e.configurationFailureNext, id)
		}
	}
	if len(e.configurationFailureNext) >= 256 {
		return true
	}
	if e.configurationFailureNext == nil {
		e.configurationFailureNext = make(map[string]time.Time)
	}
	e.configurationFailureNext[key] = now.Add(30 * time.Second)
	slog.WarnContext(ctx, "Review candidate wait history unavailable", "project_id", o.ProjectID, "operation_id", o.ID, "channel", o.Channel, "reason", o.Channel+"_wait_history_unavailable")
	return true
}

func (e *Engine) processNext(ctx context.Context, drivers map[string]Driver) error {
	if !e.beginDispatchWork() {
		return nil
	}
	defer e.endDispatchWork()
	probeDrafts := e.draftProbePermit.CompareAndSwap(false, true)
	releaseProbe := func() {
		if probeDrafts {
			e.draftProbePermit.Store(false)
			probeDrafts = false
		}
	}
	defer releaseProbe()
	candidates, err := e.pendingSchedulingCandidates(ctx, probeDrafts)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		// Candidate selection is read-only. A bounded pool/SQLite contention
		// failure has no action or journal outcome to reconcile; the existing
		// worker timer may try this local read again.
		var failure *core.Error
		if errors.As(core.NormalizeStorageError(err), &failure) && failure.Code == "overloaded" {
			return nil
		}
		return persistenceFailureAt("pending_read", err, "", "")
	}
	// Rotate from lightweight identities before lazy dependency validation.
	// A damaged selected wait consumes this pass's probe choice, not another
	// project's eligibility or the next pass's position.
	probeID := ""
	if probeDrafts {
		waits := make([]Operation, 0, len(candidates))
		for _, candidate := range candidates {
			waits = append(waits, Operation{ID: candidate.operation, Channel: candidate.channel, Reason: candidate.reason})
		}
		probeID = e.nextDraftProbe(waits)
		if probeID == "" {
			releaseProbe()
		}
	}
	for _, candidate := range candidates {
		if candidate.channel == "cli" && candidate.reason == cliDraftWait && candidate.operation != probeID {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if candidate.operation != probeID {
			releaseProbe()
		}
		o, eligible, err := e.pendingCandidate(ctx, candidate.pendingCandidateID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if automaticProjectRefused(err) || e.skipUnavailableConfiguration(ctx, o, err) || e.skipUnavailableWait(ctx, o, err) {
				continue
			}
			var failure *core.Error
			if errors.As(core.NormalizeStorageError(err), &failure) && failure.Code == "overloaded" {
				continue
			}
			return persistenceFailureAt("pending_candidate_read", err, candidate.operation, "")
		}
		if !eligible {
			continue
		}
		proceed, draftReady, explicitCLI, err := e.checkCLIDraft(ctx, o, drivers)
		// A completed readiness probe must not hold the permit during dispatch.
		releaseProbe()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if automaticProjectRefused(err) || e.skipUnavailableConfiguration(ctx, o, err) || e.skipUnavailableWait(ctx, o, err) {
				continue
			}
			var refusal *core.Error
			if errors.As(err, &refusal) && (refusal.Code == "overloaded" || refusal.Code == "review_conflict") {
				continue
			}
			return persistenceFailureAt("draft_check", err, o.ID, "")
		}
		if !proceed {
			continue
		}
		var draftReadyRevision int64
		if draftReady {
			draftReadyRevision = o.Revision
		}
		a, err := e.startAttempt(ctx, o.ProjectID, o.ID, draftReadyRevision)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if automaticProjectRefused(err) || e.skipUnavailableConfiguration(ctx, o, err) || e.skipUnavailableWait(ctx, o, err) {
				continue
			}
			var failure *core.Error
			if errors.As(err, &failure) {
				switch failure.Code {
				case "review_busy", "review_conflict", "overloaded":
					continue
				}
			}
			return persistenceFailureAt("attempt_start", err, o.ID, "")
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
			return persistenceFailureAt("configuration_read", err, o.ID, a.ID)
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
			var recoverable *PreparationRecoveryError
			if errors.As(err, &recoverable) {
				return err
			}
			if ctx.Err() == nil {
				scheduled, scheduleErr := e.ScheduleSourceRetry(ctx, o.ProjectID, a.ID, a.Revision, err)
				if scheduleErr != nil {
					return persistenceFailureAt("source_retry_schedule", scheduleErr, o.ID, a.ID)
				}
				if scheduled {
					return nil
				}
			}
			state := "held"
			reason := preparationReason(err)
			if explicitCLI && reason == "pull_draft" {
				reason = cliDraftWait
			}
			if ctx.Err() == nil && (o.Channel == "hosted" && reason == "hosted_allowance_cooldown" || o.Channel == "cli" && reason == "allowance_exhausted") {
				_, scheduleErr := e.persistCooldown(ctx, o, a)
				if scheduleErr == nil {
					return nil
				}
				var refusal *core.Error
				if ctx.Err() == nil && !cooldownEvidenceUnavailable(o.Channel, scheduleErr) && (!errors.As(scheduleErr, &refusal) || refusal.Code != "review_cooldown_elapsed") {
					return scheduleErr
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
			if errors.As(err, &refusal) && (refusal.Code == "pull_scheduling_paused" || refusal.Code == "automation_authorization_changed") {
				// Admit recorded no intent. Release only this unused attempt and
				// retain the queued operation's original identity and wait state.
				return e.finishPreparation(a, "held", refusal.Code)
			}
			// A protective source hold may arrive during successful preparation.
			// Admit has refused before recording intent or invoking Dispatch, so
			// settle this unused reservation through the normal held transition.
			if errors.As(err, &refusal) && refusal.Code == "external_hosted_pending" {
				return e.finishPreparation(a, "held", refusal.Code)
			}
			if errors.As(err, &refusal) && refusal.Code == "review_held" {
				reason := preparationReason(err)
				if reason == "source_obligations_pending" || reason == "repository_changed" {
					return e.finishPreparation(a, "held", reason)
				}
			}
			if errors.As(err, &refusal) && roundControlRefusal(refusal.Code) {
				return e.finishPreparation(a, "held", refusal.Code)
			}
			if errors.As(err, &refusal) && (refusal.Code == "review_conflict" || refusal.Code == "validation_failed") {
				return e.finishPreparation(a, "held", "admission_changed")
			}
			return persistenceFailureAt("admission", err, o.ID, a.ID)
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
	return core.Fail("review_persistence_failed", "Review execution stopped because required local state could not be read or recorded; inspect the retained operation before restarting.", 503)
}

// Keep only bounded, stable classifications at the daemon failure boundary.
// The original driver/SQL error is deliberately neither wrapped nor serialized.
func persistenceFailureAt(stage string, cause error, operationID, attemptID string) error {
	failure := persistenceFailure().(*core.Error)
	details := map[string]string{"stage": stage, "cause": "other"}
	var normalized *core.Error
	if errors.As(core.NormalizeStorageError(cause), &normalized) {
		switch normalized.Code {
		case "overloaded", "storage_unavailable", "review_conflict", "validation_failed", "not_found", "review_held", "review_projection_missing":
			details["cause"] = normalized.Code
		}
	}
	if hostedObservationID(operationID, "review_operation_") {
		details["operation_id"] = operationID
	}
	if hostedObservationID(attemptID, "review_attempt_") {
		details["attempt_id"] = attemptID
	}
	failure.Details = details
	return failure
}

// Only unavailable provider evidence permits an operation-local hold. Do not
// turn unrelated database/context errors or a missing attempt into that claim.
func cooldownEvidenceUnavailable(channel string, err error) bool {
	if channel == "cli" {
		return errors.Is(err, sql.ErrNoRows)
	}
	return channel == "hosted" && (errors.Is(err, errAllowanceObservationAbsent) || storedAllowanceUnavailable(err) || isStoredUnavailable(err, storedHostedObservation))
}

// A proven unused cooldown is already retained before this call. Retry only
// refused local writer admission or a callback deadline that rolled back before
// Commit. An ambiguous commit still requires exact retained-state reconciliation.
func (e *Engine) persistCooldown(ctx context.Context, original Operation, a Attempt) (Attempt, error) {
	local, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result Attempt
	err := retryRunSelectionLocal(local, func() error {
		entered := false
		callbackDeadline := false
		writeErr := e.store.BackgroundWrite(local, func(ctx context.Context, tx *sql.Tx) error {
			entered = true
			var err error
			if original.Channel == "cli" {
				result, err = e.scheduleCLICooldown(ctx, tx, a.ProjectID, a.ID, a.Revision)
			} else {
				result, err = e.scheduleHostedCooldown(ctx, tx, a.ProjectID, a.ID, a.Revision)
			}
			// A nonnil callback error makes BackgroundWrite roll back without
			// invoking Commit. Only its actual child deadline is retryable; a
			// domain overload or an expired outer budget is not that proof.
			callbackDeadline = err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && local.Err() == nil
			return err
		})
		if writeErr == nil {
			e.notifyOperation(a.OperationID)
			return nil
		}
		var failure *core.Error
		if (!entered || callbackDeadline) && errors.As(writeErr, &failure) && failure.Code == "overloaded" {
			return writeErr
		}
		// A failed commit may already be present. Only the exact original timer
		// and guarded attempt transition reconcile that ambiguity without a write.
		if recovered, ok := e.recordedCooldown(local, original, a); ok {
			result = recovered
			e.notifyOperation(a.OperationID)
			return nil
		}
		if cooldownEvidenceUnavailable(original.Channel, writeErr) {
			return writeErr
		}
		if errors.As(writeErr, &failure) && failure.Code == "review_cooldown_elapsed" {
			return writeErr
		}
		return persistenceFailureAt("cooldown_schedule", writeErr, a.OperationID, a.ID)
	})
	if err != nil {
		if cooldownEvidenceUnavailable(original.Channel, err) {
			return Attempt{}, err
		}
		var failure *core.Error
		if errors.As(err, &failure) && (failure.Code == "review_persistence_failed" || failure.Code == "review_cooldown_elapsed") {
			return Attempt{}, err
		}
		return Attempt{}, persistenceFailureAt("cooldown_schedule", err, a.OperationID, a.ID)
	}
	return result, nil
}

func (e *Engine) recordedCooldown(ctx context.Context, original Operation, a Attempt) (Attempt, bool) {
	// A commit error can race supervisor shutdown. Reconcile only the exact
	// durable outcome, with a separate bounded read that survives that caller.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var result Attempt
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readAttempt(ctx, tx, a.ProjectID, a.ID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, a.ProjectID, a.OperationID)
		if err != nil {
			return err
		}
		baseline := a.ID
		if original.Wait != nil && original.Wait.BaselineAttemptID != "" {
			baseline = original.Wait.BaselineAttemptID
		}
		if current.OperationID != a.OperationID || current.ProjectID != a.ProjectID || current.Provider != a.Provider || current.Account != a.Account || current.Revision != a.Revision+1 || current.State != "held" || current.Anchor != nil || current.Intent != "" || current.ExternalID != "" || o.ID != original.ID || o.ProjectID != original.ProjectID || o.PullNumber != original.PullNumber || o.Channel != original.Channel || o.ActorID != original.ActorID || o.WorkerID != original.WorkerID || o.PolicyRevision != original.PolicyRevision || o.Revision != original.Revision+2 || o.State != "queued" || o.AttemptID != "" || o.Reason != "" || o.HeadSHA != original.HeadSHA || o.ConfigurationRevision != original.ConfigurationRevision || o.CapabilityFingerprint != original.CapabilityFingerprint || o.Wait == nil || o.Wait.BaselineAttemptID != baseline {
			return conflict()
		}
		historical, err := historicalCooldownOperation(ctx, tx, o, current)
		if err != nil {
			return err
		}
		if original.Channel == "cli" {
			record, err := readCLIAllowanceRecord(ctx, tx, historical, current)
			if err != nil || record.Observation.Remaining != 0 {
				return waitUnavailable()
			}
			due, err := cliExhaustionDeadline(ctx, tx, current)
			if err != nil || o.Wait.RetryNotBefore != due.UTC().Format(time.RFC3339Nano) {
				return conflict()
			}
		} else {
			record, err := readHostedAllowanceRecord(ctx, tx, historical, current)
			if err != nil {
				return err
			}
			if record.Input.Observation.State != "cooldown_observed" || o.Wait.RetryNotBefore != record.Input.Observation.RetryNotBefore.UTC().Format(time.RFC3339Nano) {
				return conflict()
			}
		}
		var reserved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=?)`, a.ID).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return conflict()
		}
		result = current
		return nil
	})
	return result, err == nil
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
			return Attempt{}, persistenceFailureAt("outcome_write", err, a.OperationID, a.ID)
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attempt{}, persistenceFailureAt("outcome_write", err, a.OperationID, a.ID)
		case <-timer.C:
		}
	}
	return Attempt{}, persistenceFailureAt("outcome_write", nil, a.OperationID, a.ID)
}
