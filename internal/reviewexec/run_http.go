package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// RunState follows one finite selection into its original native operation.
// It never authorizes another selection or repeats an external action.
type RunState struct {
	Selection Run        `json:"selection"`
	Operation *Operation `json:"operation,omitempty"`
}

func (e *Engine) readRunState(ctx context.Context, scope core.Scope, id string) (RunState, bool, error) {
	run, err := e.ReadRun(ctx, scope, id)
	if err != nil {
		return RunState{}, false, err
	}
	out := RunState{Selection: run}
	if run.State != "selected" {
		return out, run.State != "queued" && run.State != "selecting", nil
	}
	operation, err := e.ReadOperation(ctx, scope, run.OperationID)
	if err != nil {
		return RunState{}, false, err
	}
	if operation.ProjectID != run.ProjectID || operation.ActorID != run.ActorID || operation.WorkerID != run.WorkerID || operation.Channel != run.Channel || operation.ConfigurationRevision != run.ConfigurationRevision || operation.PolicyRevision != run.PolicyRevision {
		return RunState{}, false, core.Fail("storage_unavailable", "The retained selection link is unavailable.", 500)
	}
	out.Operation = &operation
	return out, !operationPending(operation.State), nil
}

// Both stages share one deadline. Expiring our collection budget means pending;
// cancellation of the caller remains an error and never cancels durable work.
func (e *Engine) waitRunState(ctx context.Context, scope core.Scope, id string, seconds int) (RunState, bool, error) {
	if seconds == 0 {
		return e.readRunState(ctx, scope, id)
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	read := func() (RunState, bool, error) {
		var state RunState
		var terminal bool
		err := core.RetryReadAdmission(ctx, deadline, func(readCtx context.Context) error {
			var readErr error
			state, terminal, readErr = e.readRunState(readCtx, scope, id)
			return readErr
		})
		if ctx.Err() != nil {
			return RunState{}, false, resultWaitCanceled(ctx, "The accepted selection wait was canceled; its request is retained.")
		}
		return state, terminal, err
	}
	budget, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	run, terminal, err := e.WaitRun(budget, scope, id, seconds)
	if err == nil && terminal && run.State == "selected" {
		_, _, err = e.WaitOperation(budget, scope, run.OperationID, seconds)
	}
	if err != nil {
		if errors.Is(budget.Err(), context.DeadlineExceeded) && ctx.Err() == nil || waitCapacityError(err) {
			return read()
		}
		return RunState{}, false, err
	}
	return read()
}

func waitCapacityError(err error) bool {
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "overloaded" {
		if details, ok := failure.Details.(map[string]string); ok {
			return details["reason"] == "operation_wait_capacity"
		}
	}
	return false
}

func (e *Engine) enqueueRunHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", "X-FC-Source-Refusal")
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	seconds, err := operationPreference(r, 2)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	// Raw optional values distinguish omission from explicit null.
	var wire struct {
		Channel       string          `json:"channel"`
		Pull          json.RawMessage `json:"expected_pull_number,omitempty"`
		Configuration json.RawMessage `json:"expected_configuration_revision,omitempty"`
	}
	if err = core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	request := RunRequest{Channel: wire.Channel}
	for _, field := range []struct {
		raw    json.RawMessage
		target **int64
	}{{wire.Pull, &request.ExpectedPullNumber}, {wire.Configuration, &request.ExpectedConfigurationRevision}} {
		if field.raw == nil {
			continue
		}
		var number *int64
		if json.Unmarshal(field.raw, &number) != nil || number == nil {
			core.WriteError(w, invalid())
			return
		}
		*field.target = number
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err = operationResponseDeadline(w, seconds); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(seconds))
	receipt, err := e.EnqueueRun(r.Context(), scope, key, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if seconds > 0 {
		var queued Receipt
		if json.Unmarshal(receipt, &queued) != nil {
			writeOperationWaitError(w, core.Fail("storage_unavailable", "The retained selection receipt is unavailable.", 500))
			return
		}
		state, terminal, err := e.waitRunState(r.Context(), scope, queued.ID, seconds)
		if err != nil {
			writeOperationWaitError(w, err)
			return
		}
		if terminal {
			if err := e.runSourceRefusalForResponse(r, scope, &state); err != nil {
				writeOperationWaitError(w, err)
				return
			}
			if state.Operation != nil && r.Header.Get(operationWaitHeader) != "1" {
				state.Operation.Wait = nil
			}
			e.queuedRunResponseHeader(w, r, scope, state)
			core.WriteJSON(w, http.StatusOK, state)
			return
		}
	}
	core.WriteJSON(w, http.StatusAccepted, receipt)
}

func (e *Engine) readRunHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", "X-FC-Source-Refusal")
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("selection_id")
	seconds, err := operationPreference(r, 0)
	if !identifier.MatchString(id) || err != nil {
		if err == nil {
			err = invalid()
		}
		core.WriteError(w, err)
		return
	}
	if err = operationResponseDeadline(w, seconds); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(seconds))
	state, _, err := e.waitRunState(r.Context(), scope, id, seconds)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err := e.runSourceRefusalForResponse(r, scope, &state); err != nil {
		core.WriteError(w, err)
		return
	}
	if state.Operation != nil && r.Header.Get(operationWaitHeader) != "1" {
		state.Operation.Wait = nil
	}
	e.queuedRunResponseHeader(w, r, scope, state)
	core.WriteJSON(w, http.StatusOK, state)
}

// Expand only retained child diagnostics under the same selected identities.
// Reading a run never resolves source or starts another operation.
func (e *Engine) runSourceRefusalForResponse(r *http.Request, scope core.Scope, state *RunState) error {
	header := r.Header.Get("X-FC-Source-Refusal")
	if state.Operation == nil || !sourceRefusalRequested(header) {
		return nil
	}
	op, err := e.readOperationProjection(r.Context(), scope, state.Selection.OperationID, false, true, header == "v2" || header == "v3" || header == "v4")
	if err != nil {
		return err
	}
	run := state.Selection
	if op.ProjectID != run.ProjectID || op.ActorID != run.ActorID || op.WorkerID != run.WorkerID || op.Channel != run.Channel || op.ConfigurationRevision != run.ConfigurationRevision || op.PolicyRevision != run.PolicyRevision {
		return core.Fail("storage_unavailable", "The retained selection link is unavailable.", 500)
	}
	op.SourceRefusal = sourceRefusalForResponse(op.SourceRefusal, header, op.Reason)
	state.Operation = &op
	return nil
}
