package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCLIQuotaWaitHTTPFullAndCompact(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			for _, sharedOnly := range []bool{false, true} {
				t.Run(strings.Join([]string{boolName(compact), boolName(paused), boolName(sharedOnly)}, "/"), func(t *testing.T) {
					op := sampleReviewOperation()
					op.Channel, op.State, op.AttemptID, op.Reason = "cli", "queued", "", ""
					if !sharedOnly {
						op.Wait = &client.ReviewOperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-01-01T00:10:00Z", Paused: paused}
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							handshake(w)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-FC-Review-Queued", "cli_quota_wait")
						if compact {
							_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: "queued", UpdatedAt: op.UpdatedAt, NextAction: "wait", Wait: op.Wait})
						} else {
							_ = json.NewEncoder(w).Encode(op)
						}
					}))
					defer server.Close()
					c := newClient(t, server)
					var diagnostic *client.ReviewOperationDiagnostic
					if compact {
						got, err := c.ReviewOperationStatus(t.Context(), op.ID)
						if err != nil {
							t.Fatal(err)
						}
						out := client.AgentReviewOperationStatus(got)
						if !reflect.DeepEqual(got.Wait, op.Wait) || out.NextAction != "wait" || out.AttemptID != "" {
							t.Fatal("original queued intent changed", out)
						}
						diagnostic = out.Diagnostic
					} else {
						got, err := c.ReviewOperation(t.Context(), op.ID)
						if err != nil {
							t.Fatal(err)
						}
						out := client.AgentReviewOperation(got)
						if !reflect.DeepEqual(got.Wait, op.Wait) || out.State != "queued" || out.AttemptID != "" {
							t.Fatal("original queued intent changed", out)
						}
						diagnostic = out.Diagnostic
					}
					if diagnostic == nil {
						t.Fatal("missing quota guidance")
					}
					if paused && !sharedOnly {
						if !strings.Contains(diagnostic.Explanation, "paused") || !strings.Contains(diagnostic.NextAction, "Resume or cancel this same") {
							t.Fatal(diagnostic)
						}
					} else if !strings.Contains(diagnostic.Explanation, "quota") || !strings.Contains(diagnostic.NextAction, "original request queued") {
						t.Fatal(diagnostic)
					}
					if strings.Contains(diagnostic.Explanation, "provider reported a cooldown") || strings.Contains(diagnostic.NextAction, "fresh key") {
						t.Fatal("wait was turned into a terminal/manual retry", diagnostic)
					}
					if !sharedOnly && !paused && !strings.Contains(diagnostic.Explanation, "not the provider reset") {
						t.Fatal("retry time looks like provider reset", diagnostic)
					}
				})
			}
		}
	}
}

func TestHistoricalCLIQuotaHoldRemainsTerminal(t *testing.T) {
	status := client.ReviewOperationStatus{Channel: "cli", State: "held", Reason: "allowance_exhausted", NextAction: "read_diagnostics"}
	out := client.AgentReviewOperationStatus(status)
	if out.State != "held" || out.Wait != nil || out.NextAction != "retry_later_with_new_request" {
		t.Fatal("old terminal hold reopened", out)
	}
}
