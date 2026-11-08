package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestSourceRetryNewClientReadsFullCompactWithoutQuotaClaim(t *testing.T) {
	for _, compact := range []bool{false, true} {
		op := sampleReviewOperation()
		op.Channel = "cli"
		op.State = "queued"
		op.AttemptID = ""
		op.Reason = "source_rate_limit_wait"
		op.Wait = &client.ReviewOperationWait{SourceRetry: &client.ReviewSourceRetry{AttemptID: "review_attempt_" + strings.Repeat("a", 32), ObservedAt: "2026-11-09T01:00:00.75Z", RetryNotBefore: "2026-11-09T01:01:00Z", Sequence: 1}}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			if r.Header.Get("X-FC-Operation-Wait") != "4" {
				t.Error("source wait not negotiated")
			}
			if compact {
				_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: "cli", State: "queued", Revision: op.Revision, Stage: "queued", Reason: op.Reason, UpdatedAt: op.UpdatedAt, NextAction: "wait", Wait: op.Wait})
			} else {
				_ = json.NewEncoder(w).Encode(op)
			}
		}))
		c := newClient(t, server)
		if compact {
			got, err := c.ReviewOperationStatus(t.Context(), op.ID)
			if err != nil || got.Wait == nil || got.Wait.SourceRetry == nil {
				t.Fatal(got, err)
			}
		} else {
			got, err := c.ReviewOperation(t.Context(), op.ID)
			if err != nil || got.Wait == nil || got.Wait.SourceRetry == nil {
				t.Fatal(got, err)
			}
		}
		encoded, _ := json.Marshal(client.AgentReviewOperation(op))
		if !strings.Contains(string(encoded), "GitHub") || strings.Contains(string(encoded), "quota was exhausted") {
			t.Fatal(string(encoded))
		}
		server.Close()
	}
}
