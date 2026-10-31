package historycollect

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type waiter struct {
	principal string
	changed   chan struct{}
}

func (e *Engine) notify(id string) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	for w := range e.waiters[id] {
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}
}

var errWaitLifetime = errors.New("collection wait lifetime ended")

func (e *Engine) waitContext(ctx context.Context) (context.Context, func()) {
	waitCtx, cancel := context.WithCancelCause(ctx)
	if e.waitLifetime == nil {
		return waitCtx, func() { cancel(context.Canceled) }
	}
	if e.waitLifetime.Err() != nil {
		cancel(errWaitLifetime)
	}
	stop := context.AfterFunc(e.waitLifetime, func() { cancel(errWaitLifetime) })
	return waitCtx, func() { stop(); cancel(context.Canceled) }
}

func waitCanceled(ctx context.Context) error {
	failure := &core.Error{Code: "overloaded", Message: "The collection wait was canceled; its operation is unchanged.", Status: 503}
	if errors.Is(context.Cause(ctx), errWaitLifetime) {
		failure.Details = map[string]string{"reason": "collection_wait_lifetime"}
	}
	return failure
}

// Only observer shutdown can return the already committed receipt. Caller
// cancellation and failed current authorization retain the recovery error.
func (e *Engine) acceptedWaitDetached(ctx context.Context, scope core.Scope, id string, err error) (bool, error) {
	var failure *core.Error
	if ctx.Err() != nil || !errors.As(err, &failure) || failure.Code != "overloaded" {
		return false, err
	}
	details, ok := failure.Details.(map[string]string)
	if !ok || details["reason"] != "collection_wait_lifetime" {
		return false, err
	}
	if _, authErr := e.Read(ctx, scope, id); authErr != nil {
		return false, authErr
	}
	if ctx.Err() != nil {
		return false, err
	}
	return true, nil
}

// Wait subscribes before reading, so completion between admission and the first
// read cannot be missed. Cancellation detaches only this waiter. The operation
// remains durable and reads recheck current administrator/project/worker scope.
func (e *Engine) Wait(ctx context.Context, scope core.Scope, id string, duration time.Duration) (Operation, error) {
	if ctx == nil || !operationPattern.MatchString(id) || scope.PrincipalID == "" || duration < 0 || duration > 900*time.Second {
		return Operation{}, invalid()
	}
	deadline := time.Now().Add(duration)
	waitCtx, cleanup := e.waitContext(ctx)
	defer cleanup()
	read := func() (Operation, error) {
		if waitCtx.Err() != nil {
			return Operation{}, waitCanceled(waitCtx)
		}
		var value Operation
		err := core.RetryReadAdmission(waitCtx, deadline, func(readCtx context.Context) error {
			var readErr error
			value, readErr = e.Read(readCtx, scope, id)
			return readErr
		})
		if waitCtx.Err() != nil {
			return Operation{}, waitCanceled(waitCtx)
		}
		return value, err
	}
	if duration == 0 {
		return read()
	}
	if waitCtx.Err() != nil {
		return Operation{}, waitCanceled(waitCtx)
	}
	w := &waiter{principal: scope.PrincipalID, changed: make(chan struct{}, 1)}
	e.waitMu.Lock()
	if e.waitCount >= 256 || e.principals[w.principal] >= 8 {
		e.waitMu.Unlock()
		return Operation{}, &core.Error{Code: "overloaded", Message: "Too many collection waits are open; read the retained operation.", Status: 503, Details: map[string]string{"reason": "collection_wait_capacity"}}
	}
	if e.waiters[id] == nil {
		e.waiters[id] = make(map[*waiter]struct{})
	}
	e.waiters[id][w] = struct{}{}
	e.waitCount++
	e.principals[w.principal]++
	e.waitMu.Unlock()
	defer func() {
		e.waitMu.Lock()
		defer e.waitMu.Unlock()
		delete(e.waiters[id], w)
		if len(e.waiters[id]) == 0 {
			delete(e.waiters, id)
		}
		e.waitCount--
		e.principals[w.principal]--
		if e.principals[w.principal] == 0 {
			delete(e.principals, w.principal)
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		value, err := read()
		if err != nil {
			return Operation{}, err
		}
		if value.State == "completed" || value.State == "paused" || value.State == "canceled" {
			return value, nil
		}
		select {
		case <-waitCtx.Done():
			return Operation{}, waitCanceled(waitCtx)
		case <-timer.C:
			return read()
		case <-w.changed:
		}
	}
}
