package reviewruntime

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"sync"
	"time"
)

// cacheAccountGate is an isolated composition seam, not yet a runtime lock.
// Every usage/review producer sharing an authentication home must use ONE gate,
// and retain it until all child processes are reaped, before maintenance can run.
// No daemon/producer caller adopts this seam yet. It does not fence other processes.
type cacheAccountGate struct{ mu sync.Mutex }

func (g *cacheAccountGate) withExclusive(ctx context.Context, fn func() error) error {
	if g == nil || ctx == nil || fn == nil || ctx.Err() != nil {
		return errors.New("cache custody unavailable")
	}
	if !g.mu.TryLock() {
		return errors.New("cache account busy")
	}
	defer g.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fn()
}

// observeCacheCapture verifies native settlement and exact retained capture under
// the prospective shared account gate. The result is not destructive authority.
func observeCacheCapture(ctx context.Context, g *cacheAccountGate, e *reviewexec.Engine, s *CaptureStore, capability, attempt string, now time.Time) (reviewexec.SettledCLICapture, error) {
	var result reviewexec.SettledCLICapture
	if e == nil || s == nil {
		return result, errors.New("cache capture unavailable")
	}
	err := g.withExclusive(ctx, func() error {
		first, err := e.ObserveSettledCLICapture(ctx, capability, attempt, now)
		if err != nil {
			return err
		}
		if first.AttemptID == "" {
			return nil
		}
		record, ref, err := s.Read(ctx, attempt)
		if err != nil {
			return err
		}
		if !record.Capture.Started || record.Capture.ExitCode != 0 || ref.ID != first.CaptureID || ref.SHA256 != first.SHA256 || ref.Bytes != first.Bytes {
			return errors.New("cache capture mismatch")
		}
		final, err := e.ObserveSettledCLICapture(ctx, capability, attempt, now)
		if err != nil {
			return err
		}
		if first != final || ctx.Err() != nil {
			return errors.New("cache settlement changed")
		}
		result = final
		return nil
	})
	if err != nil {
		return reviewexec.SettledCLICapture{}, err
	}
	return result, nil
}
