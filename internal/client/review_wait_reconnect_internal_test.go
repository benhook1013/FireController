package client

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResultWaitReconnectPreservesOriginalTransportBudget(t *testing.T) {
	for _, seconds := range []int{1, 2, 3} {
		before := time.Now()
		calls := 0
		if err := resumeResultWait(t.Context(), seconds, func(ctx context.Context, remaining int) error {
			calls++
			deadline, ok := ctx.Deadline()
			after := time.Now()
			if !ok || remaining != seconds || deadline.Before(before.Add(providerWaitBudget(seconds))) || deadline.After(after.Add(providerWaitBudget(seconds))) {
				t.Fatal("original total transport budget changed", seconds, deadline, before, after)
			}
			return nil
		}); err != nil || calls != 1 {
			t.Fatal(err, calls)
		}
	}
}

func TestResultWaitReconnectLimitsFailuresAndHonorsCancellation(t *testing.T) {
	for _, failure := range []*Error{{Code: "service_unavailable", Status: 500}, {Code: "overloaded", Status: 503}, {Code: "overloaded", Status: 503, Details: map[string]any{"reason": "capacity"}}, {Code: "overloaded", Status: 503, Details: map[string]any{"reason": "result_wait_lifetime_extra"}}, {Code: "forbidden", Status: 403}, {Code: "unauthorized", Status: 401}, {Code: "instance_mismatch", Status: 409}, {Code: "invalid_response"}, {Code: "canceled"}, {Code: "deadline_exceeded"}} {
		calls := 0
		err := resumeResultWait(t.Context(), 10, func(context.Context, int) error { calls++; return failure })
		if err != failure || calls != 1 {
			t.Fatal("terminal or nontransport refusal retried", failure, calls)
		}
	}
	shutdown := &Error{Code: "overloaded", Status: 503, Details: map[string]any{"reason": "result_wait_lifetime"}}
	for _, transient := range []*Error{{Code: "connection_failed"}, {Code: "service_unavailable", Status: 502}, {Code: "service_unavailable", Status: 503}, {Code: "service_unavailable", Status: 504}, shutdown} {
		calls := 0
		if err := resumeResultWait(t.Context(), 2, func(context.Context, int) error {
			calls++
			if calls == 1 {
				return transient
			}
			return nil
		}); err != nil || calls != 2 {
			t.Fatal("initial transient read did not reconnect", err, calls)
		}
	}
	for _, terminal := range []*Error{{Code: "forbidden", Status: 403}, {Code: "instance_mismatch", Status: 409}, {Code: "overloaded", Status: 503}, {Code: "invalid_response"}} {
		calls := 0
		err := resumeResultWait(t.Context(), 3, func(context.Context, int) error {
			calls++
			if calls == 1 {
				return shutdown
			}
			return terminal
		})
		if err != terminal || calls != 2 {
			t.Fatal("restart window swallowed terminal authorization/capacity/protocol failure", err, calls)
		}
	}
	calls := 0
	if err := resumeResultWait(t.Context(), 0, func(context.Context, int) error { calls++; return shutdown }); err != shutdown || calls != 1 {
		t.Fatal("immediate read retried", err, calls)
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(t.Context(), 10*time.Millisecond)
		}
		calls = 0
		err := resumeResultWait(ctx, 10, func(context.Context, int) error {
			calls++
			if !deadline {
				cancel()
			}
			return shutdown
		})
		cancel()
		var failure *Error
		code := "canceled"
		if deadline {
			code = "deadline_exceeded"
		}
		if !errors.As(err, &failure) || failure.Code != code || calls != 1 {
			t.Fatal("caller interruption retried or became quiet expiry", err, calls)
		}
	}
}
