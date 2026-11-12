package client

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync"
	"time"
)

// This trace retains only local progress, never addresses, headers or errors.
// Hooks can run concurrently and after the request returns.
type resultWaitTransportTrace struct {
	mu    sync.Mutex
	phase string
}

func (t *resultWaitTransportTrace) set(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// An early response can precede WroteRequest; do not lose that evidence.
	if t.phase != "response_started" {
		t.phase = phase
	}
}
func (t *resultWaitTransportTrace) context(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn:           func(string) { t.set("connecting") },
		TLSHandshakeStart: func() { t.set("tls_handshake") },
		GotConn:           func(httptrace.GotConnInfo) { t.set("sending_request") },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.set("awaiting_response")
			}
		},
		GotFirstResponseByte: func() { t.set("response_started") },
	})
}

func resultWaitTransportError(err error, trace *resultWaitTransportTrace, started time.Time, requested, effective, attempts int) error {
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 0 || !oneReviewOf(failure.Code, "canceled", "deadline_exceeded", "connection_failed") {
		return err
	}
	details := map[string]any{}
	if original, ok := failure.Details.(map[string]any); ok {
		for key, value := range original {
			details[key] = value
		}
	} else if failure.Details != nil {
		return err
	}
	trace.mu.Lock()
	phase := trace.phase
	trace.mu.Unlock()
	if phase == "" {
		phase = "not_started"
	}
	// Both the request and reconnect window are bounded; clamp defensively.
	elapsed := min(max(time.Since(started).Milliseconds(), 0), int64(24*time.Hour/time.Millisecond))
	details["transport"] = map[string]any{"phase": phase, "elapsed_ms": elapsed, "requested_wait_seconds": requested, "effective_wait_seconds": effective, "attempt_count": attempts}
	result := *failure
	result.Details = details
	return &result
}

type tracedResultWaitError struct {
	error
	trace     *resultWaitTransportTrace
	effective int
}

func (e *tracedResultWaitError) Unwrap() error { return e.error }

// Keep instrumentation outside the reconnect policy and the request budgets.
func resumeTracedResultWait(ctx context.Context, requested, seconds int, read func(context.Context, int) error) error {
	started := time.Now()
	trace := &resultWaitTransportTrace{}
	attempts, effective := 0, seconds
	err := resumeResultWait(ctx, seconds, func(ctx context.Context, remaining int) error {
		attempts++
		effective = remaining
		trace = &resultWaitTransportTrace{}
		if err := read(trace.context(ctx), remaining); err != nil {
			return &tracedResultWaitError{error: err, trace: trace, effective: remaining}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	// The reconnect policy can return an earlier read's error. A cancellation
	// synthesized by that policy uses the most recent observed transport facts.
	if selected, ok := err.(*tracedResultWaitError); ok {
		err, trace, effective = selected.error, selected.trace, selected.effective
	}
	return resultWaitTransportError(err, trace, started, requested, effective, attempts)
}
