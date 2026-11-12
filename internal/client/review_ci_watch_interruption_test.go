package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchFitsRemainingBudgetAfterSetupWithoutChangingResults(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		remaining             time.Duration
		requested, preference int
		superseded            bool
	}{
		{"background full wait", 0, 900, 900, false},
		{"full response margin", 910 * time.Second, 900, 900, false},
		{"short budget quiet expiry", 2 * time.Second, 900, 0, false},
		{"short budget superseded", 2 * time.Second, 900, 0, true},
		{"default fits", 15 * time.Second, 2, 2, false},
		{"explicit immediate", 15 * time.Second, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pins := ciWatchPins()[:1]
			item := client.CIWatchTargetResult{PullNumber: pins[0].PullNumber, HeadSHA: pins[0].HeadSHA, BaseSHA: pins[0].BaseSHA, State: "pending", Reason: "ci_watch_check_pending", Checks: []client.ReviewCIContext{}}
			want := client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{item}}
			if tc.superseded {
				want.State, want.TimedOut = "superseded", false
				want.Targets[0].State, want.Targets[0].Reason, want.Targets[0].ObservedHeadSHA = "superseded", "ci_watch_source_changed", strings.Repeat("f", 40)
			}
			var identities, posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					identities.Add(1)
					// Setup consumes the same caller budget as the subsequent wait.
					if tc.remaining > 0 {
						time.Sleep(20 * time.Millisecond)
					}
					handshake(w)
					return
				}
				posts.Add(1)
				var body struct {
					Targets []client.CIWatchTarget `json:"targets"`
				}
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, pins) {
					t.Error("wait changed its read identity")
				}
				if r.Header.Get("Prefer") != "wait="+strconv.Itoa(tc.preference) {
					t.Error("wait did not retain response headroom", r.Header.Get("Prefer"))
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				write(w, 200, want)
			}))
			defer service.Close()
			ctx := t.Context()
			if tc.remaining > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.remaining)
				defer cancel()
			}
			c, err := client.New(ctx, binding(t, service.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			got, err := c.WatchCI(ctx, pins, tc.requested)
			if err != nil || !reflect.DeepEqual(got, want) || identities.Load() != 1 || posts.Load() != 1 {
				t.Fatal("quiet expiry or supersession became interruption/retry", got, err, identities.Load(), posts.Load())
			}
		})
	}
}

func TestCIWatchLocalInterruptionDoesNotInventCIOrRetry(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		for _, kind := range []string{"canceled", "deadline_exceeded"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				entered := make(chan struct{})
				written, responseStarted := make(chan struct{}), make(chan struct{})
				var posts atomic.Int64
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-FC-CI-Watch", "change-v3")
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					posts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if r.Header.Get("Idempotency-Key") != "" {
						t.Error("wait became a mutation")
					}
					if phase == "body" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte("{"))
						w.(http.Flusher).Flush()
					}
					close(entered)
					<-r.Context().Done()
				}))
				defer service.Close()
				c := newClient(t, service)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				// The phase deadline is armed below, after transport observation.
				// Setup has its own bounded selects and cannot consume that deadline.
				ctx = ciWatchPhaseDeadlineContext{ctx}
				// A server flush does not prove that the client received a byte.
				// Observe the same local transport event before cancellation.
				ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
					WroteRequest: func(info httptrace.WroteRequestInfo) {
						if info.Err == nil {
							close(written)
						}
					},
					GotFirstResponseByte: func() { close(responseStarted) },
				})
				out := make(chan error, 1)
				pins := ciWatchPins()
				for i := range pins {
					pins[i].BaseSHA = strings.Repeat("b", 40)
				}
				go func() { _, err := c.WatchCI(ctx, pins, 900); out <- err }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("wait not entered")
				}
				observed := written
				if phase == "body" {
					observed = responseStarted
				}
				select {
				case <-observed:
				case <-time.After(5 * time.Second):
					t.Fatal("client transport phase not observed")
				}
				if kind == "canceled" {
					cancel(errors.New("PRIVATE_HOST_CAUSE"))
				} else {
					timer := time.AfterFunc(100*time.Millisecond, func() { cancel(context.DeadlineExceeded) })
					defer timer.Stop()
				}
				select {
				case err := <-out:
					var failure *client.Error
					if !errors.As(err, &failure) || failure.Code != kind || failure.Status != 0 || failure.OutcomeUnknown || posts.Load() != 1 {
						t.Fatal("interruption classification changed", err, posts.Load())
					}
					details, ok := failure.Details.(map[string]any)
					if !ok {
						t.Fatal("missing local diagnostic details")
					}
					transport, ok := details["transport"].(map[string]any)
					wantPhase := "awaiting_response"
					if phase == "body" {
						wantPhase = "response_started"
					}
					if !ok || transport["phase"] != wantPhase || transport["requested_wait_seconds"] != 900 || transport["attempt_count"] != 1 {
						t.Fatal("missing bounded transport facts", transport)
					}
					if elapsed, ok := transport["elapsed_ms"].(int64); !ok || elapsed < 0 || elapsed > 15000 {
						t.Fatal("invalid elapsed diagnostic", transport)
					}
					if effective, ok := transport["effective_wait_seconds"].(int); !ok || effective < 0 || effective > 900 {
						t.Fatal("invalid effective wait", transport)
					}
					encoded, marshalErr := json.Marshal(failure)
					if marshalErr != nil || strings.Contains(string(encoded), "PRIVATE_HOST_CAUSE") || strings.Contains(string(encoded), service.URL) {
						t.Fatal("transport diagnostic leaked private data")
					}
					for _, fact := range []string{"No review or CI rerun was requested", "does not establish CI failure or a changed PR head", "same PR/head/base pins", "independently verifying"} {
						if !strings.Contains(failure.Action, fact) {
							t.Fatal("missing truthful CI recovery", failure)
						}
					}
					if strings.Contains(failure.Error()+failure.Action, "PRIVATE_HOST_CAUSE") || strings.Contains(failure.Message, "superseded") {
						t.Fatal("invented cause/source facts", failure)
					}
					if kind == "canceled" && !strings.Contains(failure.Message, "client context was canceled") || kind == "deadline_exceeded" && !strings.Contains(failure.Message, "deadline expired") {
						t.Fatal("cancellation and deadline conflated", failure)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("interruption did not return")
				}
			})
		}
	}
}

func TestCIWatchReconnectRequiresEveryExplicitBase(t *testing.T) {
	for _, mode := range []string{"implicit", "mixed", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			pins := ciWatchPins()
			for i := range pins {
				pins[i].BaseSHA = strings.Repeat("b", 40)
			}
			if mode != "explicit" {
				pins[0].BaseSHA = ""
			}
			if mode == "implicit" {
				pins = pins[:1]
			}
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				var body struct {
					Targets []client.CIWatchTarget `json:"targets"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, pins) {
					t.Error("watch changed original pins")
				}
				if posts.Add(1) == 1 {
					w.WriteHeader(502)
					return
				}
				result := client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: true}
				for _, pin := range pins {
					result.Targets = append(result.Targets, client.CIWatchTargetResult{PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "pending", Reason: "ci_watch_check_pending", Checks: []client.ReviewCIContext{}})
				}
				write(w, 200, result)
			}))
			defer service.Close()
			c := newClient(t, service)
			_, err := c.WatchCI(t.Context(), pins, 2)
			if mode == "explicit" {
				if err != nil || posts.Load() != 2 {
					t.Fatal("explicit pins did not reconnect", err, posts.Load())
				}
				return
			}
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Status != 502 || !strings.Contains(failure.Action, "base was implicit") || posts.Load() != 1 {
				t.Fatal("implicit server base was lost by reconnect", err, posts.Load())
			}
		})
	}
}

// Translate the deliberately armed deadline cause into the Context contract.
// WithCancelCause owns synchronization; no mutable deadline races with HTTP.
// Budget-fitting with real WithTimeout contexts is covered separately above.
type ciWatchPhaseDeadlineContext struct{ context.Context }

func (c ciWatchPhaseDeadlineContext) Err() error {
	if c.Context.Err() != nil && context.Cause(c.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// Hide the embedded cancel context's internal identity so derived HTTP contexts
// propagate this wrapper's deadline Err rather than its cancellation mechanism.
func (c ciWatchPhaseDeadlineContext) Value(key any) any {
	return context.WithoutCancel(c.Context).Value(key)
}

func TestCIWatchImplicitBaseLocalInterruptionsRequireExplicitRestart(t *testing.T) {
	for _, kind := range []string{"canceled", "deadline_exceeded"} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%t", kind, mixed), func(t *testing.T) {
				entered := make(chan struct{})
				var posts atomic.Int64
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					posts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					close(entered)
					<-r.Context().Done()
				}))
				defer service.Close()
				pins := ciWatchPins()
				pins[0].BaseSHA = ""
				if mixed {
					pins = append(pins, client.CIWatchTarget{PullNumber: 99, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)})
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				ctx = ciWatchPhaseDeadlineContext{ctx}
				done := make(chan error, 1)
				go func() { _, err := newClient(t, service).WatchCI(ctx, pins, 300); done <- err }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("watch not entered")
				}
				if kind == "canceled" {
					cancel(context.Canceled)
				} else {
					cancel(context.DeadlineExceeded)
				}
				select {
				case err := <-done:
					var failure *client.Error
					if !errors.As(err, &failure) || failure.Code != kind || !strings.Contains(failure.Action, "Verify the original comparison") || !strings.Contains(failure.Action, "explicit base pins") || posts.Load() != 1 {
						t.Fatal("unsafe implicit restart guidance", err, posts.Load())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("interruption did not return")
				}
			})
		}
	}
}
