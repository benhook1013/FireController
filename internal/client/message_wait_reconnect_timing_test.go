package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

const messageWaitReconnectReady = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n" +
	"event: message-events\nid: ready\ndata: {\"items\":[],\"cursor\":\"ready\",\"has_more\":false,\"timed_out\":false}\n\n"
const messageWaitReconnectUnavailable = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"

// In-memory connections keep transport blocking inside the synctest clock.
// Real sockets would prevent deterministic advancement of the wait budget.
func messageWaitReconnectClient(t *testing.T, serve func(int64, net.Conn)) (*Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	transport := newTransport()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				t.Error(err)
				return
			}
			defer request.Body.Close()
			n := calls.Add(1)
			cursor := "original"
			if n > 1 {
				cursor = "ready"
			}
			if request.Method != "GET" || request.URL.Path != "/api/v1/projects/project-one/message-events/stream" || request.URL.Query().Get("cursor") != cursor || request.URL.Query().Get("limit") != "1" || request.Header.Get("Authorization") != "Bearer synthetic-token" || request.Header.Get("X-FC-Instance") != "instance-one" || request.Header.Get("X-FC-Worker") != "worker-one" || request.Header.Get("Idempotency-Key") != "" {
				t.Error("reattachment changed authenticated read or validated cursor")
			}
			serve(n, server)
		}()
		return client, nil
	}
	base, _ := url.Parse("http://127.0.0.1:1")
	c := &Client{base: base, identity: profile.Context{InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, token: "synthetic-token", credentialFile: "synthetic-credential-path", transport: transport, http: &http.Client{Transport: transport}}
	return c, &calls
}

func TestMessageWaitReconnectKeepsDeadlineAndBinding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls := messageWaitReconnectClient(t, func(n int64, server net.Conn) {
			if n == 2 {
				_, _ = fmt.Fprint(server, messageWaitReconnectUnavailable)
				return
			}
			_, _ = fmt.Fprint(server, messageWaitReconnectReady)
			if n > 2 {
				_, _ = io.Copy(io.Discard, server)
			}
		})
		defer c.Close()
		start := time.Now()
		page, err := c.WaitForMessages(t.Context(), "original", 1, 1)
		if err != nil || !page.TimedOut || page.Cursor != "ready" || page.Items == nil || len(page.Items) != 0 || page.HasMore || calls.Load() != 3 || time.Since(start) != time.Second {
			t.Fatal("reconnect restarted selected timer or lost scope", page, err, calls.Load(), time.Since(start))
		}
	})
}

func TestMessageWaitReconnectCancellationAndUnrecoveredOutage(t *testing.T) {
	for _, mode := range []string{"outage", "caller-cancel", "caller-deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, calls := messageWaitReconnectClient(t, func(n int64, server net.Conn) {
					if n == 1 {
						_, _ = fmt.Fprint(server, messageWaitReconnectReady)
						return
					}
					_, _ = fmt.Fprint(server, messageWaitReconnectUnavailable)
				})
				defer c.Close()
				ctx := t.Context()
				code, status, elapsed, attempts := "service_unavailable", http.StatusServiceUnavailable, time.Second, int64(3)
				if mode == "caller-cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					time.AfterFunc(100*time.Millisecond, cancel)
					code, status, elapsed, attempts = "canceled", 0, 100*time.Millisecond, 1
				}
				if mode == "caller-deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
					defer cancel()
					code, status, elapsed, attempts = "deadline_exceeded", 0, 100*time.Millisecond, 1
				}
				start := time.Now()
				page, err := c.WaitForMessages(ctx, "original", 1, 1)
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != code || failure.Status != status || failure.OutcomeUnknown || page.Items != nil || page.Cursor != "" || page.TimedOut || page.HasMore || calls.Load() != attempts || time.Since(start) != elapsed {
					t.Fatal("outage became successful timeout or cancellation reconnected", page, err, calls.Load(), time.Since(start))
				}
			})
		})
	}
}
