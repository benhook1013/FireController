package client_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestMessageWaitPreHeaderDisconnectEndsWaitWithOriginalCursorRecovery(t *testing.T) {
	var streams atomic.Int64
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		streams.Add(1)
		if r.URL.Query().Get("cursor") != "original-cursor" {
			t.Error("caller cursor changed")
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
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "connection_failed" || failure.OutcomeUnknown || failure.Status != 0 || ctx.Err() != nil {
		t.Fatal("disconnect did not promptly end the read", err, ctx.Err())
	}
	for _, part := range []string{"This wait ended", "no message subscription remains active", "original saved cursor", "bound-worker inbox", "No automatic reconnect"} {
		if !strings.Contains(failure.Message+" "+failure.Action, part) {
			t.Fatal("missing wait recovery", part, err)
		}
	}
	if strings.Contains(failure.Action, "configured service address") || page.Items != nil || page.Cursor != "" || page.TimedOut || streams.Load() != 1 {
		t.Fatal("failed wait retried, advanced cursor or claimed timeout", page, streams.Load(), err)
	}
}
