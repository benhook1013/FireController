package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type operationWaiter struct {
	principal string
	changed   chan struct{}
}

func operationPending(state string) bool {
	return state == "queued" || state == "preparing" || state == "posting" || state == "active"
}

// Notifications are coalesced hints emitted only after the owning transaction
// commits. Every result is read and authorized afresh; no wait owns provider work.
func (e *Engine) notifyOperation(id string) {
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	for waiter := range e.operationWaiters[id] {
		select {
		case waiter.changed <- struct{}{}:
		default:
		}
	}
}

var errResultWaitLifetime = errors.New("result wait lifetime ended")

func (e *Engine) resultWaitContext(ctx context.Context) (context.Context, func()) {
	waitCtx, cancelWait := context.WithCancelCause(ctx)
	if e.resultWaitLifetime == nil {
		return waitCtx, func() { cancelWait(context.Canceled) }
	}
	if e.resultWaitLifetime.Err() != nil {
		cancelWait(errResultWaitLifetime)
	}
	stop := context.AfterFunc(e.resultWaitLifetime, func() { cancelWait(errResultWaitLifetime) })
	return waitCtx, func() {
		stop()
		cancelWait(context.Canceled)
	}
}

func resultWaitCanceled(ctx context.Context, message string) error {
	failure := &core.Error{Code: "overloaded", Message: message, Status: 503}
	if errors.Is(context.Cause(ctx), errResultWaitLifetime) {
		failure.Details = map[string]string{"reason": "result_wait_lifetime"}
	}
	return failure
}

// A committed POST already owns its receipt. Only a daemon observer shutdown
// may detach to that known response; caller cancellation and failed fresh
// authorization keep the recovery path. Existing in-flight cancellation
// normalization still wins over a simultaneous earlier read error: the receipt
// claims acceptance, not a successful observation. Reauthorize before exposing ID.
func (e *Engine) acceptedWaitDetached(ctx context.Context, scope core.Scope, err error) (bool, error) {
	var failure *core.Error
	if ctx.Err() != nil || !errors.As(err, &failure) || failure.Code != "overloaded" {
		return false, err
	}
	details, ok := failure.Details.(map[string]string)
	if !ok || details["reason"] != "result_wait_lifetime" {
		return false, err
	}
	if authErr := e.store.Read(ctx, scope, func(context.Context, *sql.Tx) error { return nil }); authErr != nil {
		return false, authErr
	}
	if ctx.Err() != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) WaitOperation(ctx context.Context, scope core.Scope, id string, waitSeconds int) (Operation, bool, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(id) || !identifier.MatchString(scope.PrincipalID) || waitSeconds < 0 || waitSeconds > 900 {
		return Operation{}, false, invalid()
	}
	waitCtx, cleanupWait := e.resultWaitContext(ctx)
	defer cleanupWait()
	canceled := func() error {
		return resultWaitCanceled(waitCtx, "The review result wait was canceled; the retained operation is unchanged.")
	}
	read := func() (Operation, error) {
		if waitCtx.Err() != nil {
			return Operation{}, canceled()
		}
		value, err := e.ReadOperation(waitCtx, scope, id)
		if waitCtx.Err() != nil {
			return Operation{}, canceled()
		}
		return value, err
	}
	if waitSeconds == 0 {
		value, err := read()
		return value, err == nil && !operationPending(value.State), err
	}
	if waitCtx.Err() != nil {
		return Operation{}, false, canceled()
	}
	waiter := &operationWaiter{principal: scope.PrincipalID, changed: make(chan struct{}, 1)}
	e.operationMu.Lock()
	if e.operationWaiterCount >= 256 || e.operationWaiterPrincipals[waiter.principal] >= 8 {
		e.operationMu.Unlock()
		return Operation{}, false, &core.Error{Code: "overloaded", Message: "Too many review result waits are open; read the retained operation.", Status: 503, Details: map[string]string{"reason": "operation_wait_capacity"}}
	}
	if e.operationWaiters == nil {
		e.operationWaiters = map[string]map[*operationWaiter]struct{}{}
		e.operationWaiterPrincipals = map[string]int{}
	}
	if e.operationWaiters[id] == nil {
		e.operationWaiters[id] = map[*operationWaiter]struct{}{}
	}
	e.operationWaiters[id][waiter] = struct{}{}
	e.operationWaiterCount++
	e.operationWaiterPrincipals[waiter.principal]++
	e.operationMu.Unlock()
	defer func() {
		e.operationMu.Lock()
		defer e.operationMu.Unlock()
		delete(e.operationWaiters[id], waiter)
		if len(e.operationWaiters[id]) == 0 {
			delete(e.operationWaiters, id)
		}
		e.operationWaiterCount--
		e.operationWaiterPrincipals[waiter.principal]--
		if e.operationWaiterPrincipals[waiter.principal] == 0 {
			delete(e.operationWaiterPrincipals, waiter.principal)
		}
	}()
	timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer timer.Stop()
	for {
		value, err := read()
		if err != nil {
			return Operation{}, false, err
		}
		if !operationPending(value.State) {
			return value, true, nil
		}
		select {
		case <-waitCtx.Done():
			return Operation{}, false, canceled()
		case <-timer.C:
			value, err = read()
			return value, err == nil && !operationPending(value.State), err
		case <-waiter.changed:
		}
	}
}

func operationPreference(r *http.Request, defaultSeconds int) (int, error) {
	values, present := r.Header["Prefer"]
	if !present {
		return defaultSeconds, nil
	}
	if len(values) != 1 || !strings.HasPrefix(values[0], "wait=") {
		return 0, invalid()
	}
	raw := strings.TrimPrefix(values[0], "wait=")
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 || seconds > 900 || strconv.Itoa(seconds) != raw {
		return 0, invalid()
	}
	return seconds, nil
}

func operationResponseDeadline(w http.ResponseWriter, seconds int) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(seconds+5) * time.Second))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return core.Fail("internal_error", "The review result response deadline could not be established; no action was accepted.", 500)
	}
	return nil
}

func writeOperationWaitError(w http.ResponseWriter, err error) {
	failure := core.Error{Code: "internal_error", Message: "The accepted review result could not be read; retain the same request and key for recovery.", Status: 500}
	var original *core.Error
	if errors.As(err, &original) {
		failure = *original
		failure.Message = "The accepted review result is unavailable; retain the same request and key for recovery."
	}
	failure.Details = map[string]any{"outcome_unknown": true}
	core.WriteError(w, &failure)
}
