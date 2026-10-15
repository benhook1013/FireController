package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// Preparation is the adapter's verified exact target and action body. Preparing
// may read providers and local Git, but must not post or spend a review.
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

// Run supervises two finite work slots, with account and target reservations
// enforced durably by StartAttempt. No request handler waits for these workers.
// The owner starts Run once before accepting requests and waits for shutdown.
func (e *Engine) Run(ctx context.Context, drivers map[string]Driver) error {
	if !e.running.CompareAndSwap(false, true) {
		return conflict()
	}
	defer e.running.Store(false)
	owned := make(map[string]Driver, len(drivers))
	for id, d := range drivers {
		if d == nil {
			return invalid()
		}
		if _, ok := e.capabilities[id]; !ok {
			return invalid()
		}
		owned[id] = d
	}
	for id := range e.capabilities {
		if owned[id] == nil {
			return invalid()
		}
	}
	if err := e.RecoverInterrupted(ctx); err != nil {
		return err
	}
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			timer := time.NewTimer(0)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-e.wake:
				case <-timer.C:
				}
				if ctx.Err() != nil {
					return
				}
				e.processNext(ctx, owned)
				timer.Reset(500 * time.Millisecond)
			}
		})
	}
	workers.Wait()
	return nil
}

func (e *Engine) pending(ctx context.Context) ([]Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := e.store.DB().QueryContext(ctx, `SELECT project_id,id FROM review_operations WHERE state='queued' ORDER BY created_at,id LIMIT 256`)
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
	// Snapshot local records in one short read transaction; there is no provider
	// activity or process waiting under a database transaction.
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]Operation, 0, len(ids))
	for _, i := range ids {
		o, err := readOperation(ctx, tx, i.project, i.operation)
		if err != nil {
			return nil, err
		}
		if o.State == "queued" {
			out = append(out, o)
		}
	}
	return out, nil
}

func (e *Engine) processNext(ctx context.Context, drivers map[string]Driver) {
	operations, err := e.pending(ctx)
	if err != nil {
		return
	}
	for _, o := range operations {
		if ctx.Err() != nil {
			return
		}
		a, err := e.StartAttempt(ctx, o.ProjectID, o.ID)
		if err != nil || a.ID == "" {
			continue
		}
		// The operation retained its exact configuration revision; StartAttempt
		// has already checked that this is still the current configuration.
		c, err := e.configurationRevision(ctx, o.ProjectID, o.ConfigurationRevision)
		if err != nil {
			e.finishPreparation(a, "held", "configuration_unavailable")
			return
		}
		driver := drivers[c.CapabilityID]
		if driver == nil {
			e.finishPreparation(a, "held", "capability_unavailable")
			return
		}
		prepareCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		prepared, err := driver.Prepare(prepareCtx, o, c, a)
		cancel()
		if err != nil {
			state := "held"
			reason := "preparation_failed"
			if ctx.Err() != nil {
				state = "cancelled"
				reason = "supervisor_stopping"
			}
			e.finishPreparation(a, state, reason)
			return
		}
		admitted, err := e.Admit(ctx, o.ProjectID, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			e.finishPreparation(a, "held", "admission_changed")
			return
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
			next, err := e.Advance(progressCtx, o.ProjectID, a.ID, a.Revision, "active", "trigger_recorded", externalID)
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
			writeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			next, advanceErr := e.Advance(writeCtx, o.ProjectID, a.ID, a.Revision, "active", "trigger_recorded", result.ExternalID)
			stop()
			if advanceErr != nil {
				return
			}
			a = next
		}
		if !allowedTransition(a.State, result.State) {
			result = Result{State: "uncertain", Reason: "adapter_result_incomplete"}
		}
		writeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = e.Advance(writeCtx, o.ProjectID, a.ID, a.Revision, result.State, result.Reason, result.ExternalID)
		stop()
		return
	}
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

func (e *Engine) finishPreparation(a Attempt, state, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = e.Advance(ctx, a.ProjectID, a.ID, a.Revision, state, reason, "")
}
