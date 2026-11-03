package client_test

import (
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOverloadedReadPreservesSafeTransientRecoveryWithoutChangingIdentity(t *testing.T) {
	var reads atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		reads.Add(1)
		if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("read failure changed operation")
		}
		write(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "overloaded", "message": "PRIVATE backend failure"}, "request_id": "request-original"})
	}))
	defer s.Close()
	c := newClient(t, s)
	_, err := c.WaitReviewOperationStatus(t.Context(), executionOperationID, 0)
	var got *client.Error
	if !errors.As(err, &got) || got.Code != "overloaded" || got.Status != 503 || got.RequestID != "request-original" || got.OutcomeUnknown {
		t.Fatal(got, err)
	}
	if !strings.Contains(got.Message, "temporarily") || !strings.Contains(got.Action, "same read") || strings.Contains(got.Message+got.Action, "PRIVATE") || strings.Contains(got.Action, "current access") {
		t.Fatal("misleading or unsafe read recovery", got)
	}
	if reads.Load() != 1 {
		t.Fatal("read error started automatic polling", reads.Load())
	}
}
