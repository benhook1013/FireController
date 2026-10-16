package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

// Transport timers may fire before the enclosing command timer. They remain
// deadlines, and dispatched mutations retain uncertainty in either ordering.
func TestTransportTimeoutBeforeContextRetainsDeadlineClassification(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		for _, mutation := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/read", true: "/mutation"}[mutation], func(t *testing.T) {
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if phase == "body" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(200)
						_, _ = w.Write([]byte("{"))
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				}))
				defer service.Close()
				base, err := url.Parse(service.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := &http.Transport{ResponseHeaderTimeout: 20 * time.Millisecond}
				defer transport.CloseIdleConnections()
				c := &Client{identity: profile.Context{InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, base: base, token: "synthetic-timeout-credential", transport: transport, http: &http.Client{Transport: transport, Timeout: 80 * time.Millisecond}}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				var result map[string]string
				if mutation {
					err = c.mutate(ctx, "POST", c.projectPath("jobs"), "explicit-timeout-key", map[string]string{"title": "Bounded request"}, 200, &result)
				} else {
					err = c.get(ctx, c.projectPath("workers"), nil, true, &result)
				}
				var failure *Error
				if ctx.Err() != nil || !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || failure.OutcomeUnknown != mutation {
					t.Fatalf("transport timeout classification: context=%v error=%+v", ctx.Err(), err)
				}
			})
		}
	}
}
