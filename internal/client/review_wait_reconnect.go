package client

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func resultWaitShutdown(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable || failure.Code != "overloaded" {
		return false
	}
	details, ok := failure.Details.(map[string]any)
	return ok && details["reason"] == "result_wait_lifetime"
}

func resultWaitReconnectFailure(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) {
		return false
	}
	return resultWaitShutdown(err) || failure.Status == 0 && failure.Code == "connection_failed" || failure.Code == "service_unavailable" && (failure.Status == 502 || failure.Status == 503 || failure.Status == 504)
}

// A transient connection/gateway failure opens this reconnection window. Every callback is
// the same authenticated read with a reduced Prefer wait, never a submission.
// A trailing fractional second permits one final immediate read, not polling.
func resumeResultWait(ctx context.Context, seconds int, read func(context.Context, int) error) error {
	if seconds == 0 {
		return read(ctx, 0)
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	outer, cancel := context.WithTimeout(ctx, providerWaitBudget(seconds))
	defer cancel()
	err := read(outer, seconds)
	if err == nil || !resultWaitReconnectFailure(err) {
		return err
	}
	interruption := err
	delay := 250 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		remaining := time.Until(deadline)
		if remaining > 0 {
			if err := waitIdentityRetry(outer, min(delay, remaining)); err != nil {
				return contextError(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		remaining = time.Until(deadline)
		if remaining > 0 && remaining < time.Second {
			if err := waitIdentityRetry(outer, remaining); err != nil {
				return contextError(err)
			}
		}
		secondsLeft := int(max(0, time.Until(deadline)) / time.Second)
		err = read(outer, secondsLeft)
		if err == nil {
			return nil
		}
		if !resultWaitReconnectFailure(err) {
			return err
		}
		if resultWaitShutdown(err) {
			interruption = err
		}
		if secondsLeft == 0 {
			return interruption
		}
		delay = min(delay*2, 2*time.Second)
	}
}
