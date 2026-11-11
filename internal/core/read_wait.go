package core

import (
	"context"
	"time"
)

// RetryReadAdmission keeps a quiet result wait open across refused read-only
// transaction admission. Each callback must freshly authorize its result and
// perform no writes or provider work. Zero deadline means one ordinary read.
// The fixed selected deadline bounds retry starts; the normal final fresh read
// may still use its existing bounded response headroom. No cached result is used.
func RetryReadAdmission(ctx context.Context, deadline time.Time, read func(context.Context) error) error {
	delay := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := read(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !IsReadAdmissionOverload(err) || deadline.IsZero() || !time.Now().Before(deadline) {
			return err
		}
		timer := time.NewTimer(min(delay, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			return err
		}
		delay = min(delay*2, time.Second)
	}
}
