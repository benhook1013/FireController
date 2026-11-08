package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCIWatchTransportFactsAreLocalBoundedAndPrivate(t *testing.T) {
	trace := &resultWaitTransportTrace{}
	hooks := httptrace.ContextClientTrace(trace.context(t.Context()))
	hooks.GetConn("PRIVATE_ADDRESS")
	hooks.TLSHandshakeStart()
	hooks.GotConn(httptrace.GotConnInfo{})
	hooks.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("PRIVATE_ERROR")})
	original := &Error{Code: "connection_failed", Message: "safe", Details: map[string]any{"retained": "fact"}}
	got := resultWaitTransportError(original, trace, time.Now().Add(-48*time.Hour), 300, 42, 3).(*Error)
	transport := got.Details.(map[string]any)["transport"].(map[string]any)
	want := map[string]any{"phase": "sending_request", "elapsed_ms": int64(24 * time.Hour / time.Millisecond), "requested_wait_seconds": 300, "effective_wait_seconds": 42, "attempt_count": 3}
	if !reflect.DeepEqual(transport, want) || got.Code != original.Code || got.Details.(map[string]any)["retained"] != "fact" {
		t.Fatal("changed error or trace", got)
	}
	if _, ok := original.Details.(map[string]any)["transport"]; ok {
		t.Fatal("mutated original error")
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "PRIVATE") {
		t.Fatal("private trace leaked")
	}
	hooks.GotFirstResponseByte()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			hooks.WroteRequest(httptrace.WroteRequestInfo{})
			_ = resultWaitTransportError(original, trace, time.Now(), 300, 42, 3)
		})
	}
	wg.Wait()
	trace.mu.Lock()
	phase := trace.phase
	trace.mu.Unlock()
	if phase != "response_started" {
		t.Fatal("late write hid response", phase)
	}
}

func TestCIWatchTransportPreservesServerAndUnrecognizedErrors(t *testing.T) {
	for _, err := range []error{errors.New("untyped"), &Error{Code: "deadline_exceeded", Status: 503}, &Error{Code: "invalid_response"}, &Error{Code: "canceled", Details: []string{"future"}}} {
		if got := resultWaitTransportError(err, &resultWaitTransportTrace{}, time.Now(), 0, 0, 0); got != err {
			t.Fatal("changed unsupported error", got)
		}
	}
	got := resultWaitTransportError(&Error{Code: "canceled"}, &resultWaitTransportTrace{}, time.Now().Add(time.Hour), 0, 0, 1).(*Error)
	facts := got.Details.(map[string]any)["transport"].(map[string]any)
	if facts["phase"] != "not_started" || facts["elapsed_ms"] != int64(0) {
		t.Fatal("invented request progress", facts)
	}
}

func TestTracedResultWaitReportsFinalAttemptWithoutChangingReconnect(t *testing.T) {
	calls := 0
	err := resumeTracedResultWait(t.Context(), 10, 3, func(ctx context.Context, seconds int) error {
		calls++
		hooks := httptrace.ContextClientTrace(ctx)
		if calls == 1 {
			if seconds != 3 {
				t.Fatal("changed first wait", seconds)
			}
			hooks.GotFirstResponseByte()
			return &Error{Code: "connection_failed"}
		}
		hooks.GetConn("PRIVATE_ADDRESS")
		return &Error{Code: "canceled"}
	})
	failure, ok := err.(*Error)
	if !ok || failure.Code != "canceled" || calls != 2 {
		t.Fatal("changed reconnect policy", err, calls)
	}
	facts := failure.Details.(map[string]any)["transport"].(map[string]any)
	if facts["phase"] != "connecting" || facts["attempt_count"] != 2 || facts["requested_wait_seconds"] != 10 {
		t.Fatal("previous attempt leaked progress", facts)
	}
	if effective, ok := facts["effective_wait_seconds"].(int); !ok || effective < 0 || effective >= 3 {
		t.Fatal("effective wait not reduced", facts)
	}
}
