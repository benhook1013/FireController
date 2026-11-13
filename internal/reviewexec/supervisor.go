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

// supervise owns two finite work slots. A shared probe permit lets either idle
// worker probe retained Draft waits without occupying both workers. Provider
// waits never hold a database transaction; startup recovery is already complete.
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
	return e.pendingForWorker(ctx, true)
}

type pendingScanPosition struct{ createdAt, id string }

func (e *Engine) pendingForWorker(ctx context.Context, probeDrafts bool) ([]Operation, error) {
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
	args := make([]any, 0, 4*len(e.capabilities))
	for _, capability := range e.capabilities {
		args = append(args, capability.ID, capability.Provider, capability.Account, capability.Fingerprint)
	}
	args = append(args, now.UTC().Format(time.RFC3339Nano))

	// Only matching retained bindings can be skipped as busy. Missing/changed
	// capabilities remain candidates for StartAttempt's normal hold handling.
	query := `WITH installed(id,provider,account,fingerprint) AS (VALUES (NULL,NULL,NULL,NULL)` + strings.Repeat(`,(?,?,?,?)`, len(e.capabilities)) + `)
	SELECT o.project_id,o.id,o.channel,o.capability_fingerprint,o.created_at FROM review_operations o
	JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
	JOIN projects p ON p.id=o.project_id
	LEFT JOIN installed i ON i.id=CASE WHEN json_valid(c.body_json) THEN json_extract(c.body_json,'$.capability_id') END AND i.fingerprint=o.capability_fingerprint
	WHERE o.state='queued'
	AND NOT EXISTS (SELECT 1 FROM review_pull_scheduling s WHERE s.project_id=o.project_id AND s.pull_number=o.pull_number AND s.paused=1)
	AND NOT EXISTS (SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND (w.paused=1 OR (w.retry_not_before<>'' AND julianday(w.retry_not_before)>julianday(?))))
	AND NOT EXISTS (SELECT 1 FROM review_execution_config latest WHERE latest.project_id=o.project_id AND latest.revision=(SELECT MAX(revision) FROM review_execution_config WHERE project_id=o.project_id) AND json_valid(latest.body_json) AND json_extract(latest.body_json,'$.paused')=1)
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.account=i.account AND r.action_class=o.channel AND (o.channel='cli' OR r.repository_id=p.github_repository_id))
	AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r WHERE r.provider=i.provider AND r.repository_id=p.github_repository_id AND r.pull_number=o.pull_number AND r.action_class=o.channel)`
	type configurationSnapshot struct {
		configuration Configuration
		revision      int64
	}
	configurations := map[string]configurationSnapshot{}
	type accountKey struct{ provider, account string }
	cliRetryDeadlines := map[accountKey]time.Time{}
	cliUnavailable := map[accountKey]bool{}
	emptyPredecessors := map[queuePriorityKey]bool{}
	readCandidate := func(project, operation string) (Operation, error) {
		o, err := readOperation(ctx, tx, project, operation)
		if err != nil {
			if automaticProjectRefused(err) || e.skipUnavailableWait(ctx, o, err) {
				return Operation{}, nil
			}
			return Operation{}, err
		}
		if o.State != "queued" {
			return Operation{}, nil
		}
		configuration, ok := configurations[o.ProjectID]
		if !ok {
			c, revision, err := readConfiguration(ctx, tx, o.ProjectID)
			if err != nil {
				if e.skipUnavailableConfiguration(ctx, o, err) || automaticProjectRefused(err) {
					return Operation{}, nil
				}
				return Operation{}, err
			}
			configuration = configurationSnapshot{c, revision}
			configurations[o.ProjectID] = configuration
		}
		c, revision := configuration.configuration, configuration.revision
		// Filter damaged retained history before returning this candidate. Otherwise
		// a partial scan can rewind to an intent that final admission always skips.
		// Ordinary incompatibility still reaches StartAttempt's hold handling.
		if _, err := compatibleQueuedConfiguration(ctx, tx, o, c, revision); err != nil {
			if e.skipUnavailableConfiguration(ctx, o, err) {
				return Operation{}, nil
			}
			return Operation{}, err
		}
		if o.Channel == "cli" {
			if capability, ok := e.capabilities[c.CapabilityID]; ok && capability.Fingerprint == o.CapabilityFingerprint {
				key := accountKey{capability.Provider, capability.Account}
				if cliUnavailable[key] {
					return Operation{}, nil
				}
				due, cached := cliRetryDeadlines[key]
				if !cached {
					due, err = cliAccountRetryDeadline(ctx, tx, capability, now)
					if err != nil {
						if e.skipUnavailableWait(ctx, o, err) {
							cliUnavailable[key] = true
							return Operation{}, nil
						}
						return Operation{}, err
					}
					cliRetryDeadlines[key] = due
				}
				if now.Before(due) {
					return Operation{}, nil
				}
			}
		}
		earlier, err := e.earlierQueuedReviewIntentInSnapshot(ctx, tx, o, c, revision, emptyPredecessors)
		if err != nil {
			if automaticProjectRefused(err) || e.skipUnavailableConfiguration(ctx, o, err) || e.skipUnavailableWait(ctx, o, err) {
				return Operation{}, nil
			}
			return Operation{}, err
		}
		if earlier != nil {
			return Operation{}, nil
		}
		return o, nil
	}
	// Draft probes use their own window so a full ordinary queue cannot
	// conceal a ready Draft, and Drafts cannot fill the dispatch window.
	var out []Operation
	if probeDrafts {
		e.draftProbeMu.Lock()
		cursor := e.draftProbeCursor
		e.draftProbeMu.Unlock()
		draftArgs := append(append([]any(nil), args...), cursor, cliDraftProbeCapacity+1)
		rows, err := tx.QueryContext(ctx, query+` AND o.channel='cli' AND o.reason='cli_draft_wait' ORDER BY o.id<=?,o.id LIMIT ?`, draftArgs...)
		if err != nil {
			return nil, err
		}
		drafts := make([]Operation, 0, cliDraftProbeCapacity+1)
		for rows.Next() {
			o := Operation{Channel: "cli", Reason: cliDraftWait}
			if err := rows.Scan(&o.ProjectID, &o.ID, &o.Channel, &o.CapabilityFingerprint, &o.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			drafts = append(drafts, o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if selected := e.nextDraftProbe(drafts); selected != "" {
			for _, draft := range drafts {
				if draft.ID == selected {
					o, err := readCandidate(draft.ProjectID, draft.ID)
					if err != nil {
						return nil, err
					}
					if o.ID != "" {
						out = append(out, o)
					}
					break
				}
			}
		}
	}
	// Each supervisor worker retains only a scan position, never eligibility.
	// Advance before validating a row so a single expensive blocked candidate
	// cannot restart the same prefix after every bounded-snapshot timeout.
	worker := 0
	if probeDrafts {
		worker = 1
	}
	e.pendingScanMu.Lock()
	cursor := e.pendingScanCursor[worker]
	e.pendingScanMu.Unlock()
	advance := func(position pendingScanPosition) {
		e.pendingScanMu.Lock()
		e.pendingScanCursor[worker] = position
		e.pendingScanMu.Unlock()
	}
	ordinaryArgs := append(append([]any(nil), args...), cursor.createdAt, cursor.createdAt, cursor.id)
	rows, err := tx.QueryContext(ctx, query+` AND NOT (o.channel='cli' AND o.reason='cli_draft_wait') AND (o.created_at>? OR (o.created_at=? AND o.id>?)) ORDER BY o.created_at,o.id`, ordinaryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Apply the dispatch bound after filtering known waits and unavailable
	// accounts. The read transaction supplies the overall scan deadline.
	limit := len(out) + 256
	position := cursor
	var firstReturned *pendingScanPosition
	resumeReturned := func() {
		if firstReturned != nil {
			advance(*firstReturned)
		}
	}
	for len(out) < limit && rows.Next() {
		var project, operation, channel, fingerprint, createdAt string
		if err := rows.Scan(&project, &operation, &channel, &fingerprint, &createdAt); err != nil {
			advance(cursor)
			return nil, err
		}
		before := position
		position = pendingScanPosition{createdAt, operation}
		advance(position)
		if channel == "cli" {
			if cfg, ok := configurations[project]; ok {
				if capability, ok := e.capabilities[cfg.configuration.CapabilityID]; ok && capability.Fingerprint == fingerprint {
					key := accountKey{capability.Provider, capability.Account}
					if cliUnavailable[key] || now.Before(cliRetryDeadlines[key]) {
						continue
					}
				}
			}
		}
		o, err := readCandidate(project, operation)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				advance(cursor) // Never rotate past an operational storage failure.
			}
			if len(out) != 0 && errors.Is(err, context.DeadlineExceeded) {
				resumeReturned()
				return out, nil
			}
			return nil, err
		}
		if o.ID != "" {
			if firstReturned == nil {
				firstReturned = &before
			}
			out = append(out, o)
		}
	}
	if err := rows.Err(); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			advance(cursor)
		}
		if len(out) != 0 && errors.Is(err, context.DeadlineExceeded) {
			resumeReturned()
			return out, nil
		}
		return nil, err
	}
	if len(out) < limit {
		advance(pendingScanPosition{})
	} else {
		// Selection processes at most one returned candidate. Do not rotate past
		// the unprocessed remainder merely because the scan filled its batch.
		resumeReturned()
	} // End of snapshot: wrap to oldest next pass.
	return out, nil
}

// preparationReason exposes only selected stable classifications, never adapter
// diagnostics, paths, provider response text or arbitrary error details.
func preparationReason(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) {
		if failure.Code == "review_evidence_pending" || failure.Code == "review_capacity" || failure.Code == "pull_scheduling_paused" || failure.Code == "automation_authorization_changed" {
			return failure.Code
		}
		if details, ok := failure.Details.(map[string]string); failure.Code == "review_held" && ok {
			switch reason := details["reason"]; reason {
			case "pull_draft", "source_changed":
				return reason
			case "source_obligations_pending", "repository_changed", "allowance_exhausted", "allowance_unavailable", "target_already_covered", "predecessor_identity_changed", "predecessor_unavailable", "predecessor_unfinished", "execution_binding_changed", "preparation_duplicate", "source_unavailable", "workspace_unavailable", "workspace_changed", "comparison_unavailable", "project_file_limit", "provider_activity_unproven", "provider_review_active", "provider_actor_unproven", "hosted_allowance_unproven", "hosted_allowance_cooldown", "standing_baseline_unproven":
				return reason
			}
		}
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
	probe := e.draftProbePermit.CompareAndSwap(false, true)
	release := func() {
		if probe {
			e.draftProbePermit.Store(false)
			probe = false
		}
	}
	defer release()
	return e.processNextWithProbe(ctx, drivers, probe, release)
}

func (e *Engine) processNextForWorker(ctx context.Context, drivers map[string]Driver, probeDrafts bool) error {
	return e.processNextWithProbe(ctx, drivers, probeDrafts, func() {})
}

func (e *Engine) processNextWithProbe(ctx context.Context, drivers map[string]Driver, probeDrafts bool, releaseProbe func()) error {
	operations, err := e.pendingForWorker(ctx, probeDrafts)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return persistenceFailure()
	}
	for _, o := range operations {
		if ctx.Err() != nil {
			return nil
		}
		proceed, draftReady, explicitCLI, err := e.checkCLIDraft(ctx, o, drivers)
		// Selection includes at most one Draft intent, ahead of ordinary work.
		// Its bounded source read is over; never retain the permit during a
		// potentially long preparation or dispatch.
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
			return persistenceFailure()
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
			return persistenceFailure()
		}
		if a.ID == "" {
			continue
		}
		// The operation retained its exact configuration revision; StartAttempt
		// has already checked that this is still the current configuration.
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
		prepareCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		prepared, err := driver.Prepare(prepareCtx, o, c, a)
		cancel()
		if err != nil {
			var recoverable *PreparationRecoveryError
			if errors.As(err, &recoverable) {
				return err
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
			return e.finishPreparation(a, state, reason)
		}
		admitted, err := e.Admit(ctx, o.ProjectID, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			if ctx.Err() != nil {
				return e.finishPreparation(a, "cancelled", "supervisor_stopping")
			}
			if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
				// Only proven retained project corruption is local. No intent
				// was admitted; retain the queued identity for a repaired read.
				return e.finishPreparation(a, "held", "automatic_scheduling_unavailable")
			}
			var refusal *core.Error
			if errors.As(err, &refusal) && (refusal.Code == "pull_scheduling_paused" || refusal.Code == "automation_authorization_changed") {
				// Admit recorded no intent. Release only this unused attempt and
				// retain the queued operation's original identity and wait state.
				return e.finishPreparation(a, "held", refusal.Code)
			}
			if errors.As(err, &refusal) && refusal.Code == "review_held" {
				reason := preparationReason(err)
				if reason == "source_obligations_pending" || reason == "repository_changed" {
					return e.finishPreparation(a, "held", reason)
				}
			}
			if errors.As(err, &refusal) && (refusal.Code == "review_conflict" || refusal.Code == "validation_failed") {
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
func (e *Engine) persistAdvance(a Attempt, state, reason, externalID string) (Attempt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for attempt := range 3 {
		next, err := e.Advance(ctx, a.ProjectID, a.ID, a.Revision, state, reason, externalID)
		if err == nil {
			return next, nil
		}
		tx, readErr := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if readErr == nil {
			current, lookupErr := readAttempt(ctx, tx, a.ProjectID, a.ID)
			var recordedReason string
			if lookupErr == nil {
				lookupErr = tx.QueryRowContext(ctx, `SELECT reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, current.Revision).Scan(&recordedReason)
			}
			_ = tx.Rollback()
			wantID := externalID
			if wantID == "" {
				wantID = a.ExternalID
			}
			if lookupErr == nil && current.Revision == a.Revision+1 && current.State == state && current.ExternalID == wantID && recordedReason == reason {
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
