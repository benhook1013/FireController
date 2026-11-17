package reviewprocess

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCacheProducerAdmissionDeadlineAndNoUnsafeRetry(t *testing.T) {
	for _, kind := range []string{"resume", "cancel", "unsafe"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			calls := 0
			bad := errors.New("changed generation")
			err := waitCacheAdmission(ctx, func() (bool, error) {
				calls++
				switch kind {
				case "cancel":
					cancel()
					return false, nil
				case "unsafe":
					return false, bad
				}
				return calls == 2, nil
			})
			switch kind {
			case "resume":
				if err != nil || calls != 2 {
					t.Fatal(err, calls)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatal(err, calls)
				}
			case "unsafe":
				if !errors.Is(err, bad) || calls != 1 {
					t.Fatal(err, calls)
				}
			}
		})
	}
}
func TestCacheProducerWaitsForMaintenanceWithoutExtendingDeadline(t *testing.T) {
	c := &CacheCustody{}
	c.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.acquireProducer(ctx) }()
	deadline := time.Now().Add(time.Second)
	for c.producerWaiters.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.producerWaiters.Load() != 1 {
		c.mu.Unlock()
		t.Fatal("producer not waiting")
	}
	select {
	case err := <-done:
		c.mu.Unlock()
		t.Fatal("producer returned before release", err)
	default:
	}
	c.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.mu.RUnlock()
	if c.producerWaiters.Load() != 0 {
		t.Fatal("waiter leaked")
	}
}
