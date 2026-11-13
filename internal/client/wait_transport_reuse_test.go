package client_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestWaitTransportReuseAcrossOwnedAndBorrowedClients(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "shared"}[pooled], func(t *testing.T) {
			var connections, closed atomic.Int64
			var mu sync.Mutex
			var observed []string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				mu.Lock()
				observed = append(observed, r.Header.Get("Authorization")+"|"+r.Header.Get("X-FC-Worker")+"|"+r.URL.Path)
				mu.Unlock()
				value := sampleReviewOperation()
				if r.Header.Get("X-FC-Worker") == "worker-two" {
					value.ProjectID = "project-two"
				}
				write(w, 200, value)
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
				if state == http.StateClosed {
					closed.Add(1)
				}
			}
			srv.Start()
			defer srv.Close()
			b := binding(t, srv.URL)
			var c *client.Client
			var err error
			pool := client.NewTransportPool()
			defer pool.Close()
			if pooled {
				c, err = client.NewWithTransportPool(t.Context(), b, pool)
			} else {
				c, err = client.New(t.Context(), b)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for range 2 {
				if _, err = c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
					t.Fatal(err)
				}
			}
			if connections.Load() != 2 {
				t.Fatal("repeat waits did not reuse separate connection", connections.Load())
			}
			if _, err = c.WaitReviewOperation(t.Context(), executionOperationID, 4); err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 3 {
				t.Fatal("different deadline shared transport settings", connections.Load())
			}
			if _, err = c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 3 {
				t.Fatal("original timeout lost idle transport")
			}
			if pooled {
				c.Close()
				other := binding(t, srv.URL)
				other.Context.ProjectID = "project-two"
				other.Context.WorkerID = "worker-two"
				if err = os.WriteFile(other.CredentialFile, []byte("synthetic-second-bearer-token-123456"), 0600); err != nil {
					t.Fatal(err)
				}
				next, err := client.NewWithTransportPool(t.Context(), other, pool)
				if err != nil {
					t.Fatal(err)
				}
				defer next.Close()
				if _, err = next.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
					t.Fatal(err)
				}
				if connections.Load() != 3 {
					t.Fatal("borrowed client close discarded shared wait connection", connections.Load())
				}
				mu.Lock()
				last := observed[len(observed)-1]
				mu.Unlock()
				if last != "Bearer synthetic-second-bearer-token-123456|worker-two|/api/v1/projects/project-two/reviews/operations/"+executionOperationID {
					t.Fatal("pooled transport leaked prior binding")
				}
			}
			if pooled {
				pool.Close()
			} else {
				c.Close()
			}
			until := time.Now().Add(2 * time.Second)
			for closed.Load() != connections.Load() && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if closed.Load() != connections.Load() {
				t.Fatal("owner close retained wait connections", closed.Load(), connections.Load())
			}

		})
	}
}

func TestWaitTransportLeaseDoesNotQueueOrStarveOrdinaryReads(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		write(w, 200, sampleReviewOperation())
	}))
	defer srv.Close()
	pool := client.NewTransportPool()
	defer pool.Close()
	b := binding(t, srv.URL)
	c, err := client.NewWithTransportPool(t.Context(), b, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	results := make(chan error, 8)
	for range 8 {
		go func() { _, e := c.WaitReviewOperation(t.Context(), executionOperationID, 3); results <- e }()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("lease queued behind active waits")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ordinary, err := client.NewWithTransportPool(ctx, b, pool)
	if err != nil {
		t.Fatal("ordinary identity read starved", err)
	}
	ordinary.Close()
	// Closing the owner retires idle entries, not accepted in-flight reads.
	pool.Close()
	unblock()
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWaitTransportFailedCallPreservesBoundsAndRecovers(t *testing.T) {
	var mode atomic.Int64
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if mode.Load() == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		if mode.Load() == 2 {
			write(w, 401, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Not authorized."}, "request_id": "request-wait-cache"})
			return
		}
		write(w, 200, sampleReviewOperation())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	if _, err := c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.WaitReviewOperation(ctx, executionOperationID, 3)
	checkError(t, err, "deadline_exceeded")
	if time.Since(started) > time.Second {
		t.Fatal("cached transport extended caller deadline")
	}
	<-entered
	mode.Store(0)
	if _, err = c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
		t.Fatal("failed lease poisoned next call", err)
	}
	mode.Store(2)
	_, err = c.WaitReviewOperation(t.Context(), executionOperationID, 3)
	checkError(t, err, "unauthorized")
	mode.Store(0)
	if _, err = c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTransportWarmReadKeepsNativeDeadline(t *testing.T) {
	var block atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if block.Load() {
			<-r.Context().Done()
			return
		}
		write(w, 200, sampleReviewOperation())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	if _, err := c.WaitReviewOperation(t.Context(), executionOperationID, 3); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	started := time.Now()
	_, err := c.WaitReviewOperation(t.Context(), executionOperationID, 3)
	elapsed := time.Since(started)
	checkError(t, err, "deadline_exceeded")
	if elapsed < 7500*time.Millisecond || elapsed > 10*time.Second {
		t.Fatal("native wait+5 deadline changed", elapsed)
	}
}

func TestWaitTransportMutationsKeepIndependentLifetime(t *testing.T) {
	var connections atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		op := sampleReviewOperation()
		op.WorkerID = "worker-one"
		op.State = "completed"
		write(w, 200, op)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c := newClient(t, srv)
	for range 2 {
		if _, err := c.RequestReview(t.Context(), "same-fixture-key", operationWaitRequest(), 3); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 3 {
		t.Fatal("mutation result waits entered shared cache", connections.Load())
	}
}
