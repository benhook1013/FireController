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

// Slow bootstrap leaves a bounded result-collection preference while preserving
// one exact mutation and genuine uncertainty when a server ignores that bound.
func TestSlowBootstrapMutationWaitFitsCallerBudgetAndRemainsSingleShot(t *testing.T) {
	for _, budget := range []struct {
		name  string
		value time.Duration
	}{
		{"review", ReviewOperationBudget(50)}, {"source-refresh", SourceRefreshBudget(50)},
	} {
		for _, scenario := range []struct {
			name               string
			callerBudget       time.Duration
			selected, wantWait int
			ignorePreference   bool
			wantElapsed        time.Duration
		}{
			{"shared-budget", 55 * time.Second, 50, 42, false, 49750 * time.Millisecond},
			{"short-caller", 10 * time.Second, 50, 0, false, 7750 * time.Millisecond},
			{"ample-caller", 70 * time.Second, 50, 50, false, 57750 * time.Millisecond},
			{"immediate", 55 * time.Second, 0, 0, false, 7750 * time.Millisecond},
			{"no-caller-deadline", 0, 50, 50, false, 57750 * time.Millisecond},
			{"server-ignores-preference", 55 * time.Second, 50, 42, true, 54750 * time.Millisecond},
		} {
			t.Run(budget.name+"/"+scenario.name, func(t *testing.T) {
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
								if r.Method != http.MethodPost || r.Header.Get("Prefer") != fmt.Sprintf("wait=%d", scenario.wantWait) || r.Header.Get("Idempotency-Key") != "original-key" || string(body) != `{"request":"original"}` {
									t.Error("mutation request changed")
								}
								// Model committed work returning a receipt after the forwarded wait.
								var serverWait int
								if _, err := fmt.Sscanf(r.Header.Get("Prefer"), "wait=%d", &serverWait); err != nil {
									t.Error("invalid forwarded wait", err)
									return
								}
								if scenario.ignorePreference {
									serverWait = scenario.selected
								}
								time.Sleep(time.Duration(serverWait) * time.Second)
								io.WriteString(server, "HTTP/1.1 202 Accepted\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
								return
							}
						}()
						return client, nil
					}}
					defer transport.CloseIdleConnections()
					base, _ := url.Parse("http://synthetic.invalid")
					c := &Client{base: base, token: "synthetic-token", credentialFile: "synthetic-credential-path", identity: profile.Context{InstanceID: "instance-one", WorkerID: "worker-one"}, transport: transport, http: &http.Client{Transport: transport, Timeout: requestTimeout}}
					ctx, cancel := context.WithCancel(t.Context())
					if scenario.callerBudget > 0 {
						cancel()
						ctx, cancel = context.WithTimeout(t.Context(), scenario.callerBudget)
					}
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
					var status int
					options := &sourceWaitOptions{seconds: scenario.selected, status: &status}
					err := c.requestBoundedOptions(ctx, http.MethodPost, []string{"api", "v1", "projects", "project-one", "reviews", "requests"}, nil, true, "original-key", []byte(`{"request":"original"}`), http.StatusAccepted, true, new(any), maxResponseBytes, decodeResponse, options)
					elapsed := time.Since(start)
					<-mutationDone
					if scenario.ignorePreference {
						var failure *Error
						if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || !failure.OutcomeUnknown || !strings.Contains(failure.Action, "exact same idempotency key") {
							t.Fatal("genuine lost receipt lost uncertainty", err)
						}
					} else if err != nil || status != http.StatusAccepted {
						t.Fatal("bounded mutation did not return its definite receipt", err, status)
					}
					if writes.Load() != 1 || elapsed != scenario.wantElapsed || options.seconds != scenario.selected {
						t.Fatal("collection changed original selection or single-shot dispatch", writes.Load(), elapsed, options.seconds)
					}
				})
			})
		}
	}
}
