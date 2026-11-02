package client_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageWaitPreHeaderDisconnectReconnectsWithOriginalCursor(t *testing.T) {
	var streams atomic.Int64
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := streams.Add(1)
		if r.URL.Query().Get("cursor") != "original-cursor" {
			t.Error("caller cursor changed")
		}
		if n > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(waitFrame(waitPage("recovered", waitEvent(7, "message.sent")))))
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	})
	c := newClient(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	page, err := c.WaitForMessages(ctx, "original-cursor", 1, 30)
	if err != nil || len(page.Items) != 1 || page.Cursor != "recovered" || page.TimedOut || streams.Load() != 2 || ctx.Err() != nil {
		t.Fatal("pre-header disconnect did not recover within original budget", page, streams.Load(), err)
	}
}
