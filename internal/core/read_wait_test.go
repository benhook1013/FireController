package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryReadAdmissionOnlyRetriesProvenReadAdmission(t *testing.T) {
	admission := &readAdmissionOverload{Fail("overloaded", "Local admission", 503)}
	for _, test := range []struct {
		name string
		err  error
	}{
		{"ordinary-overload", Fail("overloaded", "Ordinary overload", 503)},
		{"reasoned-overload", &Error{Code: "overloaded", Status: 503, Details: map[string]string{"reason": "operation_wait_capacity"}}},
		{"unauthorized", Fail("unauthorized", "Revoked", 401)},
		{"storage", Fail("storage_unavailable", "Storage unavailable", 500)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := RetryReadAdmission(t.Context(), time.Now().Add(time.Minute), func(context.Context) error { calls++; return test.err })
			if err != test.err || calls != 1 {
				t.Fatal("non-admission error was retried", err, calls)
			}
		})
	}
	calls := 0
	err := RetryReadAdmission(t.Context(), time.Now().Add(time.Second), func(context.Context) error {
		calls++
		if calls < 3 {
			return admission
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatal("proven read admission did not recover", err, calls)
	}
}

func TestRetryReadAdmissionOriginalDeadlineAndCancellation(t *testing.T) {
	admission := &readAdmissionOverload{Fail("overloaded", "Local admission", 503)}
	for _, deadline := range []time.Time{{}, time.Now().Add(-time.Second)} {
		calls := 0
		err := RetryReadAdmission(t.Context(), deadline, func(context.Context) error { calls++; return admission })
		if err != admission || calls != 1 {
			t.Fatal("immediate/final read was retried", err, calls)
		}
	}
	started := time.Now()
	deadline := started.Add(40 * time.Millisecond)
	calls := 0
	err := RetryReadAdmission(t.Context(), deadline, func(context.Context) error { calls++; return admission })
	if err != admission || calls != 1 || time.Since(started) > time.Second {
		t.Fatal("read admission reset the original deadline", err, calls, time.Since(started))
	}
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RetryReadAdmission(ctx, time.Now().Add(time.Hour), func(context.Context) error { close(entered); return admission })
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation did not interrupt read backoff", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left read retry waiting")
	}
}
