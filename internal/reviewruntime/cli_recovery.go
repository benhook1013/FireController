package reviewruntime

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// StartCLIRecovery owns local publication/settlement of already retained final
// CLI evidence. No code path here launches a process or contacts a provider.
func (d *CLIDriver) StartCLIRecovery(ctx context.Context) (<-chan error, error) {
	if ctx == nil || d == nil || d.engine == nil || !d.recovering.CompareAndSwap(false, true) {
		return nil, core.Fail("validation_failed", "Start one retained CLI recovery worker for the configured capability.", 400)
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer d.recovering.Store(false)
		var backoff time.Duration
		wait := d.recoveryWait
		if wait == nil {
			wait = waitHosted
		}
		for {
			if ctx.Err() != nil {
				done <- nil
				return
			}
			err := d.engine.RecoverRetainedCLI(ctx, d.capability.ID)
			if ctx.Err() != nil {
				done <- nil
				return
			}
			delay := 15 * time.Second
			if err != nil {
				var failure *core.Error
				if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &failure) || failure.Code != "overloaded" && failure.Code != "review_conflict") {
					done <- err
					return
				}
				if backoff == 0 {
					backoff = 30 * time.Second
				} else {
					backoff = min(backoff*2, 5*time.Minute)
				}
				delay = backoff
			} else {
				backoff = 0
			}
			if err := wait(ctx, delay); err != nil {
				if ctx.Err() != nil {
					err = nil
				}
				done <- err
				return
			}
		}
	}()
	return done, nil
}
