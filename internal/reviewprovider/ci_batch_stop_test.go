package reviewprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCIPreparedAccountFailureStopsPhysicalReads(t *testing.T) {
	for _, target := range []string{"/pulls/7", "/pulls/8", "/check-runs", "/statuses"} {
		for _, status := range []int{429, 503, 0} {
			t.Run(fmt.Sprintf("%s/%d", target, status), func(t *testing.T) {
				g, targets, _ := fullCountCohortFixture(t, "")
				underlying := g.httpClient.Transport
				if underlying == nil {
					underlying = http.DefaultTransport
				}
				var failed atomic.Bool
				var after atomic.Int64
				g.httpClient.Transport = actionsRoundTrip(func(r *http.Request) (*http.Response, error) {
					if failed.Load() {
						after.Add(1)
					}
					if strings.HasSuffix(r.URL.Path, target) && failed.CompareAndSwap(false, true) {
						if status == 0 {
							return nil, io.ErrUnexpectedEOF
						}
						h := http.Header{}
						h.Set("Retry-After", "120")
						return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("synthetic")), Request: r}, nil
					}
					return underlying.RoundTrip(r)
				})
				c, err := g.NewCIGateCohort(t.Context(), targets, "main")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				err = c.PrepareBatched(t.Context())
				var stopped *CIBatchAccountFailure
				var original *Error
				if !errors.As(err, &stopped) || !errors.As(err, &original) || !failed.Load() {
					t.Fatal("missing original account failure", err)
				}
				if status != 0 && (original.Status != status || original.RetryAfterSeconds != 120) {
					t.Fatal("failure identity/backoff lost", original)
				}
				if after.Load() != 0 || c.batched != nil {
					t.Fatal("post-failure I/O or consumable batch", after.Load(), c.batched)
				}
			})
		}
	}
}
func TestCIBatchFailureScopeAndExemptions(t *testing.T) {
	for _, tc := range []struct {
		code  string
		retry int64
		stop  bool
	}{{"provider_invalid_response", 0, false}, {"external_hosted_identity_unproven", 0, false}, {"provider_invalid_response", 1, true}, {"provider_unavailable", 0, true}, {"provider_observation_changed", 0, true}} {
		t.Run(tc.code+fmt.Sprint(tc.retry), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := &ciBatchStop{cancel: cancel}
			ctx = context.WithValue(ctx, ciBatchStopKey{}, s)
			cause := &Error{Code: tc.code, RetryAfterSeconds: tc.retry}
			noteCIBatchFailure(ctx, cause)
			if (ctx.Err() != nil) != tc.stop || (s.failure() != nil) != tc.stop {
				t.Fatal("wrong exception")
			}
			if tc.stop {
				noteCIBatchFailure(ctx, io.EOF)
				if s.failure() != cause {
					t.Fatal("first cause replaced")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	noteCIBatchFailure(ctx, io.EOF)
	if ctx.Err() != nil {
		t.Fatal("ordinary context changed")
	}
}
func TestCIPreparedInvalidInventoryKeepsPartner(t *testing.T) {
	var bad bool
	g, targets, _ := fullCountCohortFixtureHook(t, "", func(w http.ResponseWriter, r *http.Request) bool {
		if !bad && strings.HasSuffix(r.URL.Path, "/pulls/7") {
			bad = true
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"number":"bad"}`)
			return true
		}
		return false
	})
	c, err := g.NewCIGateCohort(t.Context(), targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.PrepareBatched(t.Context()); err != nil {
		t.Fatal("exempt malformed inventory aborted account", err)
	}
	if _, err = c.ObserveCIGate(t.Context(), targets[0], "main"); err == nil {
		t.Fatal("invalid member accepted")
	}
	o, err := c.ObserveCIGate(t.Context(), targets[1], "main")
	if err != nil || o.State != "satisfied" {
		t.Fatal("healthy partner lost", o, err)
	}
}
