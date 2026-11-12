package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
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

func TestReviewInflightRetainsOnlyValidatedOperationHandle(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		details    any
		keep       bool
	}{
		{"valid", "review_inflight", http.StatusConflict, map[string]any{"operation_id": executionOperationID, "private": "UNTRUSTED_DETAIL"}, true},
		{"missing", "review_inflight", http.StatusConflict, nil, false},
		{"wrong-kind", "review_inflight", http.StatusConflict, map[string]any{"operation_id": "review_attempt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, false},
		{"uppercase", "review_inflight", http.StatusConflict, map[string]any{"operation_id": strings.ToUpper(executionOperationID)}, false},
		{"numeric", "review_inflight", http.StatusConflict, map[string]any{"operation_id": 1}, false},
		{"wrong-status", "review_inflight", http.StatusBadRequest, map[string]any{"operation_id": executionOperationID}, true},
		{"wrong-code", "review_conflict", http.StatusConflict, map[string]any{"operation_id": executionOperationID}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews/requests") || r.Header.Get("Idempotency-Key") != "inflight-refusal" {
					t.Error("inflight refusal caused unexpected follow-up work", r.Method, r.URL.Path)
				}
				posts.Add(1)
				failure := map[string]any{"code": tc.code, "message": "Existing review request."}
				if tc.details != nil {
					failure["details"] = tc.details
				}
				write(w, tc.status, map[string]any{"error": failure, "request_id": "request-inflight"})
			}))
			defer srv.Close()
			c := newClient(t, srv)
			value, err := c.RequestReview(t.Context(), "inflight-refusal", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}, 0)
			var failure *client.Error
			if value.Receipt != nil || value.Operation != nil || !errors.As(err, &failure) || failure.Code != tc.code || failure.OutcomeUnknown || posts.Load() != 1 {
				t.Fatal("refusal was treated as acceptance or retried", value, err)
			}
			if tc.code == "review_inflight" && tc.status == http.StatusConflict {
				if !strings.Contains(failure.Action, "Do not submit another key") || strings.Contains(failure.Action, "current access") {
					t.Fatal("pending work refusal lacks safe recovery guidance", failure.Action)
				}
				if tc.keep && (!strings.Contains(failure.Action, "details.operation_id") || !strings.Contains(failure.Action, "queued-only")) {
					t.Fatal("valid handle lacks operation guidance", failure.Action)
				}
				if !tc.keep && strings.Contains(failure.Action, "details.operation_id") {
					t.Fatal("guidance refers to a missing handle", failure.Action)
				}
			} else if strings.Contains(failure.Action, "Do not submit another key") {
				t.Fatal("unrelated refusal acquired pending work guidance", failure.Action)
			}
			if tc.keep {
				details, ok := failure.Details.(map[string]any)
				if !ok || len(details) != 1 || details["operation_id"] != executionOperationID {
					t.Fatal("validated pending-operation handle was dropped", failure.Details)
				}
			} else if failure.Details != nil {
				t.Fatal("malformed or unrelated operation ID survived filtering", failure.Details)
			}
		})
	}
}

func TestCLIQueuedGuidanceAfterResumePreservesWaitPrecedence(t *testing.T) {
	source := &client.ReviewSourceRetry{AttemptID: "review_attempt_" + strings.Repeat("a", 32), ObservedAt: "2026-11-09T01:00:00.75Z", RetryNotBefore: "2026-11-09T01:01:00Z", Sequence: 1}
	quota := client.ReviewOperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-11-09T01:02:00Z"}
	sourceAndQuota, pausedQuota := quota, quota
	sourceAndQuota.SourceRetry = source
	pausedQuota.Paused = true
	for _, compact := range []bool{false, true} {
		for _, tc := range []struct {
			name, cause, explanation, action string
			wait                             *client.ReviewOperationWait
			draining                         bool
		}{
			{name: "resumed without quota", wait: &client.ReviewOperationWait{}},
			{name: "resumed with shared quota", cause: "cli_quota_wait", wait: &client.ReviewOperationWait{}, explanation: "shared provider account", action: "original request queued"},
			{name: "resumed with cooldown", cause: "cooldown", wait: &client.ReviewOperationWait{}, explanation: "cooldown deadline", action: "original request"},
			{name: "quota timer with shared quota", cause: "cli_quota_wait", wait: &quota, explanation: "not the provider reset", action: "original request queued"},
			{name: "quota timer with cooldown", cause: "cooldown", wait: &quota, explanation: "not the provider reset", action: "original request queued"},
			{name: "source retry with shared quota", cause: "cli_quota_wait", wait: &client.ReviewOperationWait{SourceRetry: source}, explanation: "GitHub rejected read-only source preparation", action: "original request"},
			{name: "source retry with cooldown", cause: "cooldown", wait: &client.ReviewOperationWait{SourceRetry: source}, explanation: "GitHub rejected read-only source preparation", action: "original request"},
			{name: "source retry before quota timer", cause: "cli_quota_wait", wait: &sourceAndQuota, explanation: "GitHub rejected read-only source preparation", action: "original request"},
			{name: "paused without timer", cause: "cli_quota_wait", wait: &client.ReviewOperationWait{Paused: true}, explanation: "operation is paused", action: "Resume/cancel"},
			{name: "paused quota timer", cause: "cli_quota_wait", wait: &pausedQuota, explanation: "CLI request is paused", action: "Resume or cancel"},
			{name: "paused source retry", cause: "cli_quota_wait", wait: &client.ReviewOperationWait{SourceRetry: source, Paused: true}, explanation: "source-rate-limited request is paused", action: "Resume or cancel"},
			{name: "source retry with queue order", cause: "queue_order", wait: &client.ReviewOperationWait{SourceRetry: source}, explanation: "earlier applicable review request", action: "original request"},
			{name: "quota timer with project pause", cause: "project_paused", wait: &quota, explanation: "Project execution is paused", action: "original request"},
			{name: "source retry with deployment drain", cause: "cli_quota_wait", wait: &client.ReviewOperationWait{SourceRetry: source}, draining: true, explanation: "draining new dispatch", action: "same operation"},
		} {
			t.Run(boolName(compact)+"/"+tc.name, func(t *testing.T) {
				op := sampleReviewOperation()
				op.Wait = tc.wait
				if op.Wait.SourceRetry != nil {
					op.Reason = "source_rate_limit_wait"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Error("reading queued guidance started a mutation", r.Method)
					}
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					if r.Header.Get("X-FC-Operation-Wait") != "6" {
						t.Error("source wait protocol changed")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-FC-Review-Queued", tc.cause)
					if tc.draining {
						w.Header().Set("X-FC-Deployment-Draining", "true")
					}
					if compact {
						_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: "queued", Reason: op.Reason, UpdatedAt: op.UpdatedAt, NextAction: "wait", Wait: op.Wait})
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
					wantAction := "wait"
					if tc.draining {
						wantAction = "wait_for_deployment"
					}
					if !reflect.DeepEqual(out.Wait, op.Wait) || out.ID != op.ID || out.State != "queued" || out.Reason != op.Reason || out.Revision != op.Revision || out.AttemptID != "" || out.NextAction != wantAction {
						t.Fatal("original queued request changed", out)
					}
					diagnostic = out.Diagnostic
				} else {
					got, err := c.ReviewOperation(t.Context(), op.ID)
					if err != nil {
						t.Fatal(err)
					}
					out := client.AgentReviewOperation(got)
					if !reflect.DeepEqual(out.Wait, op.Wait) || out.ID != op.ID || out.State != "queued" || out.Reason != op.Reason || out.Revision != op.Revision || out.AttemptID != "" || out.ConfigurationRevision != op.ConfigurationRevision {
						t.Fatal("original queued request changed", out)
					}
					diagnostic = out.Diagnostic
				}
				if tc.explanation == "" {
					if diagnostic != nil {
						t.Fatal("empty resumed wait invented guidance", diagnostic)
					}
					return
				}
				if diagnostic == nil || !strings.Contains(diagnostic.Explanation, tc.explanation) || !strings.Contains(diagnostic.NextAction, tc.action) || strings.Contains(diagnostic.NextAction, "fresh key") {
					t.Fatal("queued guidance lost its original request or precedence", diagnostic)
				}
			})
		}
	}
}
