package reviewprocess

import (
	"context"
	"errors"
	"os"
	"time"
)

var errCacheHomeBusy = errors.New("CLI HOME custody busy")

// Retain the caller's original deadline. Only admission contention is retried;
// process execution and changed HOME generations are never retried here.
func waitCacheAdmission(ctx context.Context, try func() (bool, error)) error {
	if ctx == nil || try == nil {
		return errors.New("CLI producer custody unavailable")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := try()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (c *CacheCustody) acquireProducer(ctx context.Context) error {
	if c == nil {
		return errors.New("CLI producer custody unavailable")
	}
	c.producerWaiters.Add(1)
	defer c.producerWaiters.Add(-1)
	return waitCacheAdmission(ctx, func() (bool, error) { return c.mu.TryRLock(), nil })
}
func (l *CacheHomeLease) acquireProducer(ctx context.Context) (*os.File, error) {
	var held *os.File
	err := waitCacheAdmission(ctx, func() (bool, error) {
		f, err := l.acquire(false)
		if errors.Is(err, errCacheHomeBusy) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		held = f
		return true, nil
	})
	return held, err
}
