package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestMessageWaitReadinessPreservesInterruption(t *testing.T) {
	for _, mode := range []string{"dial", "pre-header", "eof", "gateway", "latest", "caller-cancel", "caller-deadline", "no-interruption"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := newTransport()
				var calls atomic.Int64
				transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					n := calls.Add(1)
					if mode == "no-interruption" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if mode == "dial" || mode == "latest" && n == 1 {
						return nil, errors.New("PRIVATE_NETWORK_DETAIL")
					}
					client, server := net.Pipe()
					go func() {
						defer server.Close()
						request, err := http.ReadRequest(bufio.NewReader(server))
						if err != nil {
							t.Error(err)
							return
						}
						defer request.Body.Close()
						if request.Method != "GET" || request.URL.Path != "/api/v1/projects/project-one/message-events/stream" || request.URL.Query().Get("cursor") != "saved-cursor" || request.Header.Get("X-FC-Instance") != "instance-one" || request.Header.Get("X-FC-Worker") != "worker-one" || request.Header.Get("Authorization") != "Bearer synthetic-token" {
							t.Error("reconnect changed authenticated request or caller cursor")
						}
						switch mode {
						case "pre-header":
							return
						case "eof":
							_, _ = fmt.Fprint(server, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
						default:
							_, _ = fmt.Fprint(server, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
						}
					}()
					return client, nil
				}
				base, _ := url.Parse("http://127.0.0.1:1")
				c := &Client{base: base, identity: profile.Context{InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, token: "synthetic-token", transport: transport, http: &http.Client{Transport: transport}}
				defer c.Close()
				ctx := t.Context()
				want, status, elapsed := "connection_failed", 0, requestTimeout
				if mode == "gateway" || mode == "latest" {
					want, status = "service_unavailable", 503
				}
				if mode == "no-interruption" {
					want = "deadline_exceeded"
				}
				if mode == "caller-cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					time.AfterFunc(time.Second, cancel)
					want, elapsed = "canceled", time.Second
				}
				if mode == "caller-deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
					want, elapsed = "deadline_exceeded", time.Second
				}
				start := time.Now()
				page, err := c.WaitForMessages(ctx, "saved-cursor", 1, 30)
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != want || failure.Status != status || failure.OutcomeUnknown || page.Items != nil || page.TimedOut || time.Since(start) != elapsed {
					t.Fatal("readiness interruption contract", page, err, time.Since(start), calls.Load())
				}
				if strings.Contains(err.Error(), "PRIVATE") {
					t.Fatal("private transport error escaped", err)
				}
				if want == "connection_failed" || want == "service_unavailable" {
					if calls.Load() < 2 || !strings.Contains(failure.Action, "cursor") || !strings.Contains(failure.Action, "inbox") {
						t.Fatal("lost bounded reconnect recovery guidance", calls.Load(), failure)
					}
				}
			})
		})
	}
}
