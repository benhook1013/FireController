package historycollect

import (
	"context"
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

// Wait subscribes before reading, so completion between admission and the first
// read cannot be missed. Cancellation detaches only this waiter. The operation
// remains durable and reads recheck current administrator/project/worker scope.
func (e *Engine) Wait(ctx context.Context, scope core.Scope, id string, duration time.Duration) (Operation, error) {
	if ctx == nil || !operationPattern.MatchString(id) || scope.PrincipalID == "" || duration < 0 || duration > 900*time.Second {
		return Operation{}, invalid()
	}
	if duration == 0 {
		return e.Read(ctx, scope, id)
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
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		value, err := e.Read(ctx, scope, id)
		if err != nil {
			return Operation{}, err
		}
		if value.State == "completed" || value.State == "paused" || value.State == "canceled" {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return Operation{}, core.Fail("overloaded", "The collection wait was canceled; its operation is unchanged.", 503)
		case <-timer.C:
			return e.Read(ctx, scope, id)
		case <-w.changed:
		}
	}
}
