package client_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// These are controlled HTTP stream closures, not a reproduction of an unknown
// installed failure. Incomplete transport must not look like invalid JSON.
func TestMessageWaitLostHTTPStreamReconnectsFromLastValidatedCursor(t *testing.T) {
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
				n := streams.Add(1)
				wantCursor := cursor
				if n > 1 && strings.HasPrefix(test.body, ready) {
					wantCursor = "locally-validated"
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/message-events/stream" || r.URL.Query().Get("cursor") != wantCursor || r.URL.Query().Get("limit") != "1" || len(r.URL.Query()) != 2 {
					t.Error("reconnect changed binding or advanced beyond the last validated frame")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n > 1 {
					_, _ = fmt.Fprint(w, waitFrame(waitPage("recovered", waitEvent(9, "message.sent"))))
					return
				}
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
			if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 9 || page.Cursor != "recovered" || page.TimedOut || streams.Load() != 2 {
				t.Fatal("lost stream did not resume original bounded read", page, streams.Load(), err)
			}
		})
	}
}
