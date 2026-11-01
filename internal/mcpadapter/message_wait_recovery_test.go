package mcpadapter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMessageWaitSDKConnectionErrorEndsCallAndOriginalCursorCanRearm(t *testing.T) {
	for _, phase := range []string{"before-headers", "after-readiness"} {
		t.Run(phase, func(t *testing.T) {
			f := mutationFixture(t)
			mux := http.NewServeMux()
			mux.Handle("/", f.handler)
			streamContext, stopStreams := context.WithCancel(t.Context())
			defer stopStreams()
			server.RegisterMessageStream(mux, f.store, streamContext)
			var streams, writes atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
				}
				if strings.HasSuffix(r.URL.Path, "/message-events/stream") && streams.Add(1) == 1 {
					if phase == "before-headers" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
					} else {
						cursor := r.URL.Query().Get("cursor")
						page := client.MessageEventPage{Items: []client.MessageEvent{}, Cursor: cursor}
						body, _ := json.Marshal(page)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: message-events\nid: %s\ndata: %s\n\n", cursor, body)
						w.(http.Flusher).Flush()
					}
					return
				}
				mux.ServeHTTP(w, r)
			}))
			defer service.Close()
			identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.recipient}
			session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": identity}, f.credential), "chosen")
			var original client.InboxSnapshot
			decodeResult(t, call(t, session, "inbox_snapshot", nil), &original)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			failed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": original.EventCursor, "wait_seconds": 30}})
			cancel()
			if err != nil {
				t.Fatal("native wait did not return its error result", err)
			}
			failure := diagnosticResult(t, failed, "connection_failed", false)
			if !strings.Contains(failure.Message, "This wait ended") || !strings.Contains(failure.Message, "no message subscription remains active") || !strings.Contains(failure.Action, "original saved cursor") || streams.Load() != 1 {
				t.Fatal("failed wait remained active or reconnected", failure, streams.Load())
			}
			// A new message remains in the durable inbox while there is no subscription.
			providers := f.providers.Load()
			message := apiRequest(t, f.handler, "POST", "/api/v1/projects/"+f.project+"/messages", f.admin, "message-after-ended-wait", f.sender, `{"recipient_worker_id":"`+f.recipient+`","body":"Controlled pending message"}`, 201)
			var snapshot client.InboxSnapshot
			decodeResult(t, call(t, session, "inbox_snapshot", nil), &snapshot)
			if len(snapshot.Inbox.Items) != 1 || snapshot.Inbox.Items[0].ID != message["id"] || snapshot.Inbox.Items[0].Seen || snapshot.Inbox.Items[0].Acknowledged {
				t.Fatal("same SDK session could not recover pending inbox", snapshot)
			}
			// Explicit recovery uses the ORIGINAL successful cursor, even after reading
			// another snapshot. The failed tool supplied no cursor or acknowledgment.
			var page client.MessageEventPage
			decodeResult(t, call(t, session, "wait_for_messages", map[string]any{"cursor": original.EventCursor, "wait_seconds": 1}), &page)
			if page.TimedOut || len(page.Items) != 1 || page.Items[0].ResourceID != message["id"] || streams.Load() != 2 || writes.Load() != 0 || f.providers.Load() != providers {
				t.Fatal("rearm lost pending message or made extra calls", page, streams.Load(), writes.Load())
			}
			var seen, acknowledged int
			if err := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?`, message["id"]).Scan(&seen, &acknowledged); err != nil || seen != 0 || acknowledged != 0 {
				t.Fatal("recovery acknowledged message", seen, acknowledged, err)
			}
		})
	}
}
