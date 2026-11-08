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
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

// This records the current shared-budget limitation, not a repaired wait budget.
func TestSlowBootstrapSharedMutationWaitRemainsUnknownAndSingleShot(t *testing.T) {
	for _, budget := range []struct {
		name  string
		value time.Duration
	}{
		{"review", ReviewOperationBudget(50)}, {"source-refresh", SourceRefreshBudget(50)},
	} {
		t.Run(budget.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if budget.value != 55*time.Second {
					t.Fatal("update the documented shared-budget contract", budget.value)
				}
				var identities, writes atomic.Int64
				mutationDone := make(chan struct{})
				transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
					client, server := net.Pipe()
					go func() {
						defer server.Close()
						reader := bufio.NewReader(server)
						for {
							r, err := http.ReadRequest(reader)
							if err != nil {
								return
							}
							body, err := io.ReadAll(r.Body)
							r.Body.Close()
							if err != nil {
								return
							}
							if r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
								t.Error("binding changed")
							}
							if r.URL.Path == "/api/v1/instance" {
								status, payload, media := 503, "synthetic unavailable", "text/plain"
								if identities.Add(1) == 7 {
									status, payload, media = 200, `{"instance_id":"instance-one"}`, "application/json"
								}
								fmt.Fprintf(server, "HTTP/1.1 %d status\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", status, media, len(payload), payload)
								continue
							}
							writes.Add(1)
							defer close(mutationDone)
							if r.Method != http.MethodPost || r.Header.Get("Prefer") != "wait=50" || r.Header.Get("Idempotency-Key") != "original-key" || string(body) != `{"request":"original"}` {
								t.Error("mutation request changed")
							}
							// Model a committed operation whose receipt waits for the full preference.
							time.Sleep(50 * time.Second)
							io.WriteString(server, "HTTP/1.1 202 Accepted\r\nContent-Length: 2\r\n\r\n{}")
							return
						}
					}()
					return client, nil
				}}
				defer transport.CloseIdleConnections()
				base, _ := url.Parse("http://synthetic.invalid")
				c := &Client{base: base, token: "synthetic-token", credentialFile: "synthetic-credential-path", identity: profile.Context{InstanceID: "instance-one", WorkerID: "worker-one"}, transport: transport, http: &http.Client{Transport: transport, Timeout: requestTimeout}}
				ctx, cancel := context.WithTimeout(t.Context(), max(budget.value, 15*time.Second))
				defer cancel()
				start := time.Now()
				var instance struct {
					InstanceID string `json:"instance_id"`
				}
				if err := c.verifyInstance(ctx, &instance); err != nil {
					t.Fatal(err, identities.Load())
				}
				if identities.Load() != 7 || time.Since(start) != 7750*time.Millisecond {
					t.Fatal("bootstrap was not slow", identities.Load(), time.Since(start))
				}
				err := c.requestBoundedOptions(ctx, http.MethodPost, []string{"api", "v1", "projects", "project-one", "reviews", "requests"}, nil, true, "original-key", []byte(`{"request":"original"}`), http.StatusAccepted, true, new(any), maxResponseBytes, decodeResponse, &sourceWaitOptions{seconds: 50})
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || !failure.OutcomeUnknown || writes.Load() != 1 || time.Since(start) != 55*time.Second || !strings.Contains(failure.Action, "exact same idempotency key") {
					t.Fatal("shared deadline lost uncertainty or single-shot recovery", err, writes.Load(), time.Since(start))
				}
				<-mutationDone
			})
		})
	}
}
