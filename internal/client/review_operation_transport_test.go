package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestOperationReadsRetainSanitizedTransportOnInterruption(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, phase := range []string{"headers", "body"} {
			for _, kind := range []string{"canceled", "deadline_exceeded"} {
				name := phase + "/" + kind
				if compact {
					name = "compact/" + name
				} else {
					name = "full/" + name
				}
				t.Run(name, func(t *testing.T) {
					entered := make(chan struct{})
					var calls atomic.Int64
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							handshake(w)
							return
						}
						calls.Add(1)
						expected := "/api/v1/projects/project-one/reviews/operations/" + executionOperationID
						if compact {
							expected += "/status"
						}
						if r.Method != "GET" || r.URL.Path != expected || r.Header.Get("Prefer") != "wait=900" || r.Header.Get("Idempotency-Key") != "" {
							t.Error("changed read identity or budget")
						}
						if phase == "body" {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte("{"))
							w.(http.Flusher).Flush()
						}
						close(entered)
						<-r.Context().Done()
					}))
					defer srv.Close()
					c := newClient(t, srv)
					defer c.Close()
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					responseStarted := make(chan struct{}, 1)
					if phase == "body" && kind == "canceled" {
						// A server Flush does not prove that the client has observed
						// any response bytes. Wait for the same HTTP trace event that
						// defines response_started before cancelling that phase.
						ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() {
							select {
							case responseStarted <- struct{}{}:
							default:
							}
						}})
					}
					if kind == "deadline_exceeded" {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
						defer stop()
					}
					done := make(chan error, 1)
					go func() {
						var err error
						if compact {
							_, err = c.WaitReviewOperationStatus(ctx, executionOperationID, 900)
						} else {
							_, err = c.WaitReviewOperation(ctx, executionOperationID, 900)
						}
						done <- err
					}()
					select {
					case <-entered:
					case <-time.After(2 * time.Second):
						t.Fatal("read never arrived")
					}
					if kind == "canceled" {
						if phase == "body" {
							select {
							case <-responseStarted:
							case <-time.After(2 * time.Second):
								t.Fatal("client did not observe the response before cancellation")
							}
						}
						cancel(errors.New("PRIVATE_CALLER_CAUSE"))
					}
					select {
					case err := <-done:
						var failure *client.Error
						if !errors.As(err, &failure) || failure.Code != kind || failure.Status != 0 || failure.OutcomeUnknown || calls.Load() != 1 {
							t.Fatal("changed interruption or retried", err, calls.Load())
						}
						details, ok := failure.Details.(map[string]any)
						if !ok || details["operation_id"] != executionOperationID {
							t.Fatal("lost operation identity", failure)
						}
						facts, ok := details["transport"].(map[string]any)
						wantPhase := "awaiting_response"
						if phase == "body" {
							wantPhase = "response_started"
						}
						if !ok || facts["phase"] != wantPhase || facts["requested_wait_seconds"] != 900 || facts["effective_wait_seconds"] != 900 || facts["attempt_count"] != 1 {
							t.Fatal("incorrect transport facts", facts)
						}
						if elapsed, ok := facts["elapsed_ms"].(int64); !ok || elapsed < 0 || elapsed > 2000 {
							t.Fatal("invalid elapsed", facts)
						}
						encoded, _ := json.Marshal(failure)
						if strings.Contains(string(encoded), "PRIVATE_CALLER_CAUSE") || strings.Contains(string(encoded), srv.URL) {
							t.Fatal("private transport data leaked")
						}
					case <-time.After(2 * time.Second):
						t.Fatal("read failed to detach")
					}
				})
			}
		}
	}
}
