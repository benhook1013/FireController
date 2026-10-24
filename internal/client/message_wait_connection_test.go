package client_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

// These are controlled HTTP stream closures, not a reproduction of an unknown
// installed failure. Incomplete transport must not look like invalid JSON.
func TestMessageWaitLostHTTPStreamRetainsCallerCursorAndSafeRecovery(t *testing.T) {
	ready := waitFrame(waitPage("locally-validated"))
	for _, test := range []struct {
		name, body         string
		shortLength, reset bool
	}{
		{name: "eof before readiness"},
		{name: "eof after readiness", body: ready},
		{name: "eof mid frame", body: ready + "event: message-events\nid: partial\ndata: {\"items\":["},
		{name: "eof before frame terminator", body: strings.TrimSuffix(ready, "\n")},
		{name: "unexpected eof after readiness", body: ready, shortLength: true},
		{name: "unexpected eof mid frame", body: ready + "event: message-events\nid: partial\ndata: {", shortLength: true},
		{name: "transport reset after readiness", body: ready, reset: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var streams atomic.Int64
			const cursor = "caller-saved-cursor"
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				streams.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/message-events/stream" || r.URL.Query().Get("cursor") != cursor || r.URL.Query().Get("limit") != "1" || len(r.URL.Query()) != 2 {
					t.Error("stream failure triggered a write, reconnect or changed caller cursor")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if test.shortLength {
					w.Header().Set("Content-Length", fmt.Sprint(len(test.body)+100))
				}
				_, _ = fmt.Fprint(w, test.body)
				w.(http.Flusher).Flush()
				if test.reset {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if tcp, ok := connection.(*net.TCPConn); ok {
						if err = tcp.SetLinger(0); err != nil {
							t.Error(err)
						}
					}
					_ = connection.Close()
				}
			})
			c := newClient(t, s)
			page, err := c.WaitForMessages(context.Background(), cursor, 1, 1)
			checkError(t, err, "connection_failed")
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Message != "The message connection was lost." || failure.Action != "Retry the wait using the saved cursor, or read the bound-worker inbox; messages remain unacknowledged." || failure.OutcomeUnknown || failure.Status != 0 {
				t.Fatal("lost stream diagnostic leaked transport content or lost safe recovery", err)
			}
			if page.Items != nil || page.Cursor != "" || page.TimedOut || streams.Load() != 1 {
				t.Fatal("lost stream retried, advanced a cursor or returned partial success", page, streams.Load())
			}
		})
	}
}
