package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
)

func TestWaitLifetimeDiagnosticRequiresProvenServerReason(t *testing.T) {
	for _, test := range []struct {
		name, code, reason string
		status             int
		shutdown           bool
	}{
		{"shutdown", "overloaded", "result_wait_lifetime", 503, true},
		{"generic overload", "overloaded", "", 503, false},
		{"unrecognized reason", "overloaded", "result_wait_lifetime_extra", 503, false},
		{"other error", "provider_unavailable", "result_wait_lifetime", 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reads atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("wait read changed route or gained mutation key")
				}
				w.Header().Set("X-Request-ID", "request-original-wait")
				core.WriteError(w, &core.Error{Code: test.code, Status: test.status, Message: "PRIVATE_SERVER_MESSAGE", Details: map[string]any{"reason": test.reason, "private": "PRIVATE_SERVER_DETAIL"}})
			}))
			defer s.Close()
			c := newClient(t, s)
			_, err := c.WatchCI(t.Context(), []client.CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}}, 300)
			var got *client.Error
			if !errors.As(err, &got) || got.Code != test.code || got.Status != test.status || got.RequestID != "request-original-wait" || got.OutcomeUnknown || reads.Load() != 1 {
				t.Fatal("error identity/outcome changed or read retried", got, err, reads.Load())
			}
			if strings.Contains(got.Message, "shutdown") != test.shutdown {
				t.Fatal("shutdown inferred without exact server reason", got)
			}
			if test.shutdown {
				details, ok := got.Details.(map[string]any)
				if !ok || len(details) != 1 || details["reason"] != "result_wait_lifetime" || !strings.Contains(got.Action, "CI PR/head/base pins") || !strings.Contains(got.Action, "does not mean CI failed") {
					t.Fatal("known interruption lost safe reattachment guidance", got)
				}
			} else if got.Details != nil {
				t.Fatal("unknown or foreign reason was retained", got)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "PRIVATE_SERVER") {
				t.Fatal("private server text was echoed")
			}
		})
	}
}
