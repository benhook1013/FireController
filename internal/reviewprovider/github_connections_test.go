package reviewprovider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual production client, not httptest.Server.Client: local
// server fixtures otherwise hide the production connection policy entirely.
func TestGitHubProductionTransportReusesBoundedConnections(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		name := "production"
		if disabled {
			name = "former_no_keepalive"
		}
		t.Run(name, func(t *testing.T) {
			var requests, dials, attempts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/repo","default_branch":"main"}`))
			}))
			defer server.Close()
			g, err := NewGitHub("")
			if err != nil {
				t.Fatal(err)
			}
			g.baseURL = server.URL
			transport := g.httpClient.Transport.(*http.Transport)
			defer transport.CloseIdleConnections()
			if disabled {
				transport.DisableKeepAlives = true

			} else if transport.DisableKeepAlives || transport.MaxIdleConns != 8 || transport.MaxIdleConnsPerHost != 8 || transport.IdleConnTimeout != 30*time.Second {
				t.Fatal("production idle connection pool is not bounded")
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				attempts.Add(1)
				// A small controlled setup cost distinguishes repeated connection work
				// from server processing; this is not a live-provider latency estimate.
				timer := time.NewTimer(10 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
				}
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					dials.Add(1)
				}
				return conn, err
			}
			started := time.Now()
			for offset := 0; offset < 85; offset += 4 {
				var wg sync.WaitGroup
				for i := offset; i < min(offset+4, 85); i++ {
					wg.Go(func() {
						if _, err := g.Repository(t.Context(), 1); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Wait()
			}
			elapsed := time.Since(started)
			t.Logf("reads=%d connections=%d dial_attempts=%d elapsed=%s", requests.Load(), dials.Load(), attempts.Load(), elapsed)
			if requests.Load() != 85 {
				t.Fatal("fresh reads were skipped or repeated", requests.Load())
			}
			// net/http can finish a speculative dial after another connection
			// becomes idle. Allow that bounded pool warm-up, not one dial per GET.
			if disabled && dials.Load() != 85 || !disabled && dials.Load() > 8 {
				t.Fatal("unexpected connection count", dials.Load())
			}
		})
	}
}

func TestGitHubReusedConnectionDoesNotRetryUncertainMutation(t *testing.T) {
	var mutations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/repo","default_branch":"main"}`))
			return
		}
		mutations.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // Provider may have acted; response never reaches caller.
	}))
	defer server.Close()
	g, err := NewGitHub("")
	if err != nil {
		t.Fatal(err)
	}
	defer g.httpClient.CloseIdleConnections()
	g.baseURL = server.URL
	if _, err = g.Repository(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	var output any
	err = g.requestJSON(t.Context(), http.MethodPost, "/mutation", nil, []byte(`{"body":"test"}`), http.StatusCreated, &output)
	var failure *Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || mutations.Load() != 1 {
		t.Fatal("reused connection retried mutation or lost uncertainty", mutations.Load(), err)
	}
}

func TestGitHubConnectionPoolsStayAdapterScoped(t *testing.T) {
	first, _ := NewGitHub("first-token")
	second, _ := NewGitHub("second-token")
	defer first.httpClient.CloseIdleConnections()
	defer second.httpClient.CloseIdleConnections()
	if first.httpClient.Transport == second.httpClient.Transport {
		t.Fatal("authenticated adapters share a transport")
	}
}
