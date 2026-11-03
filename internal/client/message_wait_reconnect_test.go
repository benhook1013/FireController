package client_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageWaitReconnectKeepsDeadlineAndBinding(t *testing.T) {
	var calls atomic.Int64
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		want := "original"
		if n > 1 {
			want = "ready"
		}
		if r.Method != "GET" || r.URL.Query().Get("cursor") != want || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("reattachment changed authenticated read")
		}
		if n == 2 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")))
		w.(http.Flusher).Flush()
		if n > 2 {
			<-r.Context().Done()
		}
	})
	start := time.Now()
	page, err := newClient(t, s).WaitForMessages(t.Context(), "original", 1, 1)
	if err != nil || !page.TimedOut || page.Cursor != "ready" || calls.Load() != 3 || time.Since(start) < 900*time.Millisecond || time.Since(start) > 1500*time.Millisecond {
		t.Fatal("reconnect restarted selected timer or lost scope", page, err, calls.Load(), time.Since(start))
	}
}

func TestMessageWaitReconnectCancellationAndUnrecoveredOutage(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			var calls atomic.Int64
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")))
					w.(http.Flusher).Flush()
					if cancelEarly {
						cancel()
					}
					return
				}
				w.WriteHeader(503)
			})
			start := time.Now()
			page, err := newClient(t, s).WaitForMessages(ctx, "original", 1, 1)
			code := "service_unavailable"
			if cancelEarly {
				code = "canceled"
			}
			checkError(t, err, code)
			if page.Items != nil || page.TimedOut || cancelEarly && (calls.Load() != 1 || time.Since(start) > 500*time.Millisecond) || !cancelEarly && (time.Since(start) < 900*time.Millisecond || time.Since(start) > 1500*time.Millisecond) {
				t.Fatal("outage became successful timeout or cancellation reconnected", page, err, calls.Load(), time.Since(start))
			}
		})
	}
}

func TestMessageWaitReconnectPreservesAggregateBoundsAndSequence(t *testing.T) {
	for _, byteBudget := range []bool{false, true} {
		t.Run(fmt.Sprint(byteBudget), func(t *testing.T) {
			var calls atomic.Int64
			comment := ":" + strings.Repeat("x", 256<<10) + "\n\n"
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					_, _ = fmt.Fprint(w, waitFrame(waitPage("handled", waitEvent(9, "message.seen"))))
					if byteBudget {
						_, _ = fmt.Fprint(w, strings.Repeat(comment, 13))
					}
					return
				}
				if r.URL.Query().Get("cursor") != "handled" {
					t.Error("validated handling cursor lost")
				}
				if byteBudget {
					_, _ = fmt.Fprint(w, strings.Repeat(comment, 4))
				}
				_, _ = fmt.Fprint(w, waitFrame(waitPage("invalid", waitEvent(9, "message.sent"))))
			})
			page, err := newClient(t, s).WaitForMessages(t.Context(), "original", 1, 3)
			checkError(t, err, "invalid_response")
			if page.Items != nil || calls.Load() != 2 {
				t.Fatal("reconnect reset wire/sequence bounds", page, calls.Load(), err)
			}
		})
	}
}

func TestMessageWaitReconnectErrorBodiesConsumeSharedWireBudget(t *testing.T) {
	var calls atomic.Int64
	chunk := ":" + strings.Repeat("x", 4090) + "\n\n"
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")), strings.Repeat(chunk, ((4<<20)-(32<<10))/len(chunk)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = fmt.Fprint(w, `{"error":{"code":"service_unavailable","message":"restart"},"request_id":"request-outage"}`+strings.Repeat(" ", 60<<10))
	})
	page, err := newClient(t, s).WaitForMessages(t.Context(), "original", 1, 3)
	checkError(t, err, "invalid_response")
	if page.Items != nil || calls.Load() != 2 {
		t.Fatal("error response bypassed aggregate reconnect budget", page, err, calls.Load())
	}
}
