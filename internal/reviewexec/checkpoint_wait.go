package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"time"
)

type checkpointWaiter struct {
	principal string
	changed   chan struct{}
}

func (e *Engine) wakeCheckpoints() {
	select {
	case e.checkpointWake <- struct{}{}:
	default:
	}
}
func (e *Engine) notifyCheckpoint(id string) {
	e.checkpointMu.Lock()
	defer e.checkpointMu.Unlock()
	for w := range e.checkpointWaiters[id] {
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}
}

// WaitCheckpointPublication observes only committed status and reauthorizes each
// result. The boolean is true for a terminal result, false for accepted work.
func (e *Engine) WaitCheckpointPublication(ctx context.Context, scope core.Scope, id string, waitSeconds int) (CheckpointPublication, bool, error) {
	if ctx == nil || e == nil || e.store == nil || !validPublicationID(id) || !identifier.MatchString(scope.PrincipalID) || waitSeconds < 0 || waitSeconds > 60 {
		return CheckpointPublication{}, false, invalid()
	}
	waitCtx, cleanupWait := e.resultWaitContext(ctx)
	defer cleanupWait()
	// A driver interrupt can lose the context cause while a read is in flight.
	// Normalize only this caller's cancellation; preserve other read failures.
	canceled := func() error {
		return core.Fail("overloaded", "The checkpoint wait was canceled; the accepted action is unchanged.", 503)
	}
	read := func() (CheckpointPublication, error) {
		if waitCtx.Err() != nil {
			return CheckpointPublication{}, canceled()
		}
		v, err := e.ReadCheckpointPublication(waitCtx, scope, id)
		if waitCtx.Err() != nil {
			return CheckpointPublication{}, canceled()
		}
		return v, err
	}
	if waitSeconds == 0 {
		v, err := read()
		return v, err == nil && !checkpointActive(v.State), err
	}
	if waitCtx.Err() != nil {
		return CheckpointPublication{}, false, canceled()
	}
	w := &checkpointWaiter{principal: scope.PrincipalID, changed: make(chan struct{}, 1)}
	e.checkpointMu.Lock()
	if e.checkpointWaiterCount >= 256 || e.checkpointWaiterPrincipals[w.principal] >= 8 {
		e.checkpointMu.Unlock()
		return CheckpointPublication{}, false, &core.Error{Code: "overloaded", Message: "Too many checkpoint waits are open; read its retained status.", Status: 503, Details: map[string]string{"reason": "checkpoint_wait_capacity"}}
	}
	if e.checkpointWaiters[id] == nil {
		e.checkpointWaiters[id] = map[*checkpointWaiter]struct{}{}
	}
	e.checkpointWaiters[id][w] = struct{}{}
	e.checkpointWaiterCount++
	e.checkpointWaiterPrincipals[w.principal]++
	e.checkpointMu.Unlock()
	defer func() {
		e.checkpointMu.Lock()
		defer e.checkpointMu.Unlock()
		delete(e.checkpointWaiters[id], w)
		if len(e.checkpointWaiters[id]) == 0 {
			delete(e.checkpointWaiters, id)
		}
		e.checkpointWaiterCount--
		e.checkpointWaiterPrincipals[w.principal]--
		if e.checkpointWaiterPrincipals[w.principal] == 0 {
			delete(e.checkpointWaiterPrincipals, w.principal)
		}
	}()
	timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer timer.Stop()
	for {
		v, err := read()
		if err != nil {
			return CheckpointPublication{}, false, err
		}
		if !checkpointActive(v.State) {
			return v, true, nil
		}
		select {
		case <-waitCtx.Done():
			return CheckpointPublication{}, false, canceled()
		case <-timer.C:
			v, err = read()
			return v, err == nil && !checkpointActive(v.State), err
		case <-w.changed:
		}
	}
}
