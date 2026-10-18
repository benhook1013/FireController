package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/server"
)

// Exercise real HTTP/1 requests beyond the daemon's unchanged 10-second
// ReadTimeout/WriteTimeout. No provider worker runs: the fixture later records
// an ordinary local hold through the real journal transition API.
func TestReviewHTTPWaitSurvivesServerReadTimeout(t *testing.T) {
	f := newObserverTestFixture(t)
	type pending struct {
		project, method, path, key string
		body                       []byte
		receipt                    reviewexec.Receipt
		attempt                    reviewexec.Attempt
	}
	requests := []pending{
		{project: "failed", method: http.MethodGet, key: "deadline-get"},
		{project: "sibling", method: http.MethodPost, key: "deadline-post"},
	}
	for i := range requests {
		p := &requests[i]
		p.receipt = f.enqueue(t, p.project, p.key)
		var err error
		p.attempt, err = f.engine.StartAttempt(t.Context(), p.project, p.receipt.ID)
		if err != nil || p.attempt.State != "preparing" {
			t.Fatalf("prepare local journal: state=%s error=%v", p.attempt.State, err)
		}
		p.path = "/api/v1/projects/" + p.project + "/reviews/operations/" + p.receipt.ID
		if p.method == http.MethodPost {
			p.path = "/api/v1/projects/" + p.project + "/reviews/requests"
			p.body, err = json.Marshal(reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 1, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	mux := server.New(f.store)
	f.engine.Register(mux)
	entered := make(chan context.Context, len(requests))
	srv := newHTTPServer(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		mux.ServeHTTP(w, r)
	}))
	if srv.ReadTimeout != 10*time.Second || srv.WriteTimeout != 10*time.Second {
		t.Fatal("proof requires the real unchanged daemon timeout configuration")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-serveDone:
			if err != nil && err != http.ErrServerClosed {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("owned HTTP server did not join")
		}
	})
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	type result struct {
		method string
		status int
		value  reviewexec.Operation
		err    error
	}
	results := make(chan result, len(requests))
	started := time.Now()
	for _, p := range requests {
		go func() {
			r, err := http.NewRequestWithContext(t.Context(), p.method, "http://"+listener.Addr().String()+p.path, bytes.NewReader(p.body))
			if err != nil {
				results <- result{method: p.method, err: err}
				return
			}
			r.Header.Set("Authorization", "Bearer "+f.admin)
			r.Header.Set("X-FC-Instance", f.store.InstanceID())
			r.Header.Set("Prefer", "wait=30")
			if p.method == http.MethodPost {
				r.Header.Set("Idempotency-Key", p.key)
			}
			response, err := client.Do(r)
			if err != nil {
				results <- result{method: p.method, err: err}
				return
			}
			defer response.Body.Close()
			var value reviewexec.Operation
			err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&value)
			results <- result{method: p.method, status: response.StatusCode, value: value, err: err}
		}()
	}
	var contexts []context.Context
	for range requests {
		select {
		case ctx := <-entered:
			contexts = append(contexts, ctx)
		case <-time.After(2 * time.Second):
			t.Fatal("request did not enter real HTTP server")
		}
	}
	// This is deliberately longer than the actual server timeout, not a
	// shortened substitute. Both waits must remain attached until journal hold.
	timer := time.NewTimer(11 * time.Second)
	defer timer.Stop()
	select {
	case early := <-results:
		t.Fatalf("%s ended before terminal transition: status=%d error=%v", early.method, early.status, early.err)
	case <-timer.C:
	}
	for _, ctx := range contexts {
		if ctx.Err() != nil {
			t.Fatal("request context canceled by read deadline", ctx.Err())
		}
	}
	for _, p := range requests {
		if _, err := f.engine.Advance(t.Context(), p.project, p.attempt.ID, p.attempt.Revision, "held", "deadline_fixture_hold", ""); err != nil {
			t.Fatal(err)
		}
	}
	for range requests {
		select {
		case got := <-results:
			if got.err != nil || got.status != http.StatusOK || got.value.State != "held" || got.value.Reason != "deadline_fixture_hold" {
				t.Fatalf("%s terminal response: status=%d state=%s error=%v", got.method, got.status, got.value.State, got.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("terminal notification did not complete the HTTP wait")
		}
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("exact POST replay changed admitted operations: count=%d error=%v", count, err)
	}
	t.Logf("GET and fully consumed JSON POST returned terminal200 after %s with ReadTimeout/WriteTimeout10s and Preferwait30", time.Since(started))
}
