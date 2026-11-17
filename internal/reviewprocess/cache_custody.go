package reviewprocess

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// CacheCustody coordinates participating CLI children, not external processes.
// Producers may overlap. Maintenance never waits for or interrupts a producer.
// One instance must cover every runner sharing an account home. The daemon uses
// one conservative instance across all of its CLI capabilities.
type CacheCustody struct {
	mu              sync.RWMutex
	groupsMu        sync.Mutex
	groups          map[int]struct{}
	unresolved      bool
	producerWaiters atomic.Int64
}

func WithCacheCustody(c *CacheCustody) Option {
	return func(r *Runner) error {
		if c == nil {
			return processError("configuration_invalid", false)
		}
		r.cacheCustody = c
		return nil
	}
}

func (c *CacheCustody) track(pid int) {
	c.groupsMu.Lock()
	defer c.groupsMu.Unlock()
	if c.groups == nil {
		c.groups = make(map[int]struct{})
	}
	// Retire only groups the OS proves absent; uncertain groups stay fenced.
	for previous := range c.groups {
		if ownedGroupAbsent(previous) {
			delete(c.groups, previous)
		}
	}
	if len(c.groups) >= 1024 {
		c.unresolved = true
		return
	}
	c.groups[pid] = struct{}{}
}

// TryMaintenance holds custody through the synchronous callback. Successful
// admission proves only participating process groups absent at this observation,
// not native settlement, path ownership, or permission to remove any files.
func (c *CacheCustody) TryMaintenance(ctx context.Context, fn func() error) error {
	if c == nil || ctx == nil || fn == nil || ctx.Err() != nil || c.producerWaiters.Load() != 0 || !c.mu.TryLock() {
		return errors.New("CLI cache custody unavailable")
	}
	defer c.mu.Unlock()
	if c.producerWaiters.Load() != 0 {
		return errors.New("CLI producer awaiting custody")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.groupsMu.Lock()
	if c.unresolved {
		c.groupsMu.Unlock()
		return errors.New("CLI process group inventory unresolved")
	}
	for pid := range c.groups {
		if !ownedGroupAbsent(pid) {
			c.groupsMu.Unlock()
			return errors.New("CLI process group unresolved")
		}
		delete(c.groups, pid)
	}
	c.groupsMu.Unlock()
	return fn()
}
