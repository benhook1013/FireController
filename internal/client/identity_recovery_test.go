package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestIdentityPreflightRecoversOneTransientReadWithoutDispatchingWork(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/instance" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" {
					t.Error("preflight changed binding or dispatched another operation")
				}
				if calls.Add(1) == 1 {
					http.Error(w, "synthetic gateway failure", status)
					return
				}
				handshake(w)
			}))
			defer s.Close()
			c, err := client.New(t.Context(), binding(t, s.URL))
			if err != nil || calls.Load() != 2 {
				t.Fatal("bounded identity recovery", err, calls.Load())
			}
			c.Close()
		})
	}
}

func TestIdentityPreflightDoesNotRetryPermanentOrRepeatedFailures(t *testing.T) {
	for _, status := range []int{401, 403, 409, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "synthetic failure", status)
			}))
			defer s.Close()
			c, err := client.New(t.Context(), binding(t, s.URL))
			want := int64(1)
			if status == 503 {
				want = 2
			}
			if err == nil || c != nil || calls.Load() != want {
				t.Fatal("unexpected retry or accepted failure", err, calls.Load())
			}
		})
	}
}

func TestIdentityPreflightRecoveryRespectsCancellationAndInstancePin(t *testing.T) {
	for _, mode := range []string{"cancel", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if mode == "cancel" {
						cancel()
					}
					http.Error(w, "synthetic gateway failure", 503)
					return
				}
				write(w, 200, map[string]string{"instance_id": "other-instance"})
			}))
			defer s.Close()
			start := time.Now()
			c, err := client.New(ctx, binding(t, s.URL))
			code, count := "canceled", int64(1)
			if mode == "replacement" {
				code, count = "instance_mismatch", 2
			}
			checkError(t, err, code)
			if c != nil || calls.Load() != count || time.Since(start) > time.Second {
				t.Fatal("cancellation/binding recovery boundary", calls.Load())
			}
		})
	}
}
