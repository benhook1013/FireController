package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestHTTPShutdownDetachesSelectionWaitsAndKeepsAcceptedWork(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "authorized"
		if revoked {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			f := newObserverTestFixture(t)
			lifetime, cancelWaits := context.WithCancel(context.Background())
			defer cancelWaits()
			capabilities := []reviewexec.Capability{
				{ID: "failed", Provider: "coderabbit", Account: "failed", Channels: []string{"hosted"}},
				{ID: "sibling", Provider: "coderabbit", Account: "sibling", Channels: []string{"hosted"}},
			}
			engine, err := reviewexec.New(f.store, capabilities, reviewexec.WithResultWaitLifetime(lifetime))
			if err != nil {
				t.Fatal(err)
			}
			mux := server.New(f.store)
			engine.Register(mux)
			srv := newHTTPServer(context.Background(), mux)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- srv.Serve(listener) }()
			defer func() {
				_ = srv.Close()
				_ = listener.Close()
			}()

			project := "failed"
			postPath := "/api/v1/projects/sibling/reviews/selections"
			scope := f.scope(t, project)
			scope.WorkerID = "sender"
			getReceipt, err := engine.EnqueueRun(t.Context(), scope, "shutdown-get-seed", reviewexec.RunRequest{Channel: "hosted"})
			if err != nil {
				t.Fatal(err)
			}
			var getOperation reviewexec.Receipt
			if json.Unmarshal(getReceipt, &getOperation) != nil {
				t.Fatal("GET wait fixture receipt is invalid")
			}
			getPath := "/api/v1/projects/failed/reviews/selections/" + getOperation.ID
			body, err := json.Marshal(reviewexec.RunRequest{Channel: "hosted"})
			if err != nil {
				t.Fatal(err)
			}
			startRequest := func(method, path, key, data string) <-chan struct {
				status int
				body   string
			} {
				done := make(chan struct {
					status int
					body   string
				}, 1)
				go func() {
					request, requestErr := http.NewRequest(method, "http://"+listener.Addr().String()+path, strings.NewReader(data))
					if requestErr != nil {
						done <- struct {
							status int
							body   string
						}{0, requestErr.Error()}
						return
					}
					request.Header.Set("Authorization", "Bearer "+f.admin)
					request.Header.Set("X-FC-Instance", f.store.InstanceID())
					request.Header.Set("Prefer", "wait=900")
					request.Header.Set("X-FC-Worker", "sender")
					if key != "" {
						request.Header.Set("Idempotency-Key", key)
					}
					response, requestErr := (&http.Client{Timeout: 8 * time.Second}).Do(request)
					if requestErr != nil {
						done <- struct {
							status int
							body   string
						}{0, requestErr.Error()}
						return
					}
					defer response.Body.Close()
					content, _ := io.ReadAll(response.Body)
					done <- struct {
						status int
						body   string
					}{response.StatusCode, string(content)}
				}()
				return done
			}
			postDone := startRequest(http.MethodPost, postPath, "shutdown-post", string(body))
			getDone := startRequest(http.MethodGet, getPath, "", "")
			deadline := time.Now().Add(time.Second)
			admitted := false
			for time.Now().Before(deadline) {
				var count int
				if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_selections`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count == 2 {
					admitted = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !admitted {
				t.Fatal("POST selection was not admitted before shutdown")
			}
			select {
			case result := <-postDone:
				t.Fatal("long POST returned before lifecycle shutdown", result.status, result.body)
			case result := <-getDone:
				t.Fatal("long GET returned before lifecycle shutdown", result.status, result.body)
			case <-time.After(40 * time.Millisecond):
			}
			if revoked {
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1`); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			cancelWaits()
			if err = shutdownHTTPServer(srv, 5*time.Second); err != nil {
				t.Fatal("lifecycle waits forced HTTP shutdown", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatal("wait cancellation did not make daemon shutdown prompt", elapsed)
			}
			for index, done := range []<-chan struct {
				status int
				body   string
			}{postDone, getDone} {
				select {
				case result := <-done:
					var accepted reviewexec.Receipt
					postAccepted := index == 0 && result.status == http.StatusAccepted && json.Unmarshal([]byte(result.body), &accepted) == nil && accepted.State == "queued" && accepted.Revision == 1 && strings.HasPrefix(accepted.ID, "review_selection_") && !strings.Contains(result.body, "outcome_unknown")
					if revoked && index == 0 {
						postAccepted = result.status == http.StatusUnauthorized && accepted.ID == "" && !strings.Contains(result.body, "review_selection_")
					}
					getCanceled := index == 1 && result.status == http.StatusServiceUnavailable && strings.Contains(result.body, `"code":"overloaded"`) && !strings.Contains(result.body, `"state":"completed"`)
					if !postAccepted && !getCanceled {
						t.Fatal("accepted wait returned unsafe shutdown response", result.status, result.body)
					}
				case <-time.After(time.Second):
					t.Fatal("shutdown did not release a result waiter")
				}
			}
			if err = <-serveDone; !errors.Is(err, http.ErrServerClosed) {
				t.Fatal("server did not finish graceful shutdown", err)
			}
			var queued, receipts int
			if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_selections WHERE state='queued'`).Scan(&queued); err != nil || queued != 2 {
				t.Fatal("shutdown changed accepted operations", queued, err)
			}
			if err = f.store.DB().QueryRow(`SELECT count(*) FROM receipts WHERE operation='reviews.run'`).Scan(&receipts); err != nil || receipts != 2 {
				t.Fatal("shutdown changed accepted receipts", receipts, err)
			}
		})
	}
}
