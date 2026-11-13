package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/server"
)

// A quiet pending result must return before the client wait+5 budget, even
// beyond the real daemon's unchanged ten-second HTTP deadlines.
func TestReviewOperationQuietHTTPTimeoutBoundary(t *testing.T) {
	f := newObserverTestFixture(t)
	receipt := f.enqueue(t, "failed", "quiet-diagnostic")
	attempt, err := f.engine.StartAttempt(t.Context(), "failed", receipt.ID)
	if err != nil || attempt.State != "preparing" {
		t.Fatal(err)
	}
	mux := server.New(f.store)
	f.engine.Register(mux)
	srv := newHTTPServer(context.Background(), mux)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	defer func() { _ = srv.Close(); <-done }()
	req, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+listener.Addr().String()+"/api/v1/projects/failed/reviews/operations/"+receipt.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.admin)
	req.Header.Set("X-FC-Instance", f.store.InstanceID())
	req.Header.Set("Prefer", "wait=12")
	req.Header.Set("X-FC-Operation-Evidence", "true")
	req.Header.Set("X-FC-Source-Refusal", "v4")
	req.Header.Set("X-FC-Hosted-Query", "2")
	req.Header.Set("X-FC-Review-Blockers", "true")
	req.Header.Set("X-FC-Hosted-Observer", "1")
	started := time.Now()
	res, err := (&http.Client{Timeout: 17 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("elapsed=%s error=%v", time.Since(started), err)
	}
	defer res.Body.Close()
	var value reviewexec.Operation
	if err = json.NewDecoder(res.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || value.State != "preparing" || time.Since(started) < 12*time.Second {
		t.Fatalf("status%d state%s elapsed%s", res.StatusCode, value.State, time.Since(started))
	}
	t.Logf("diagnostic operation quiet wait returned 200/preparing after %s", time.Since(started))
}
