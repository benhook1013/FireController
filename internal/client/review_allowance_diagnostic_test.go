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

func TestHostedAllowanceStageDiagnosticsStrictReadsAndOriginalHandles(t *testing.T) {
	for _, reason := range []string{"hosted_allowance_history_invalid", "hosted_allowance_history_unavailable", "hosted_allowance_capture_invalid", "hosted_allowance_capture_unavailable", "hosted_allowance_query_post_unproven", "hosted_allowance_query_recording_pending", "hosted_allowance_capture_recording_pending"} {
		t.Run(reason, func(t *testing.T) {
			full := sampleReviewOperation()
			full.Channel, full.State, full.Reason, full.AttemptID = "hosted", "held", reason, "review_attempt_"+strings.Repeat("b", 32)
			full.HostedQuery = &client.ReviewHostedQueryDiagnostic{QueryID: "review_hosted_query_" + strings.Repeat("c", 32), QueryCommentID: 123, QueryURL: "https://github.com/owner/repo/pull/2#issuecomment-123"}
			compact := compactStatusFixture()
			compact.State, compact.Stage, compact.NextAction, compact.Reason = "held", "held", "read_diagnostics", reason
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads++
				if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("diagnostic requested mutation")
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/status") {
					_ = json.NewEncoder(w).Encode(compact)
				} else {
					if r.Header.Get("X-FC-Hosted-Query") != "2" {
						t.Error("query metadata opt-in lost")
					}
					_ = json.NewEncoder(w).Encode(full)
				}
			}))
			defer server.Close()
			c := newClient(t, server)
			got, err := c.ReviewOperation(t.Context(), full.ID)
			if err != nil {
				t.Fatal(err)
			}
			short, err := c.ReviewOperationStatus(t.Context(), compact.ID)
			if err != nil {
				t.Fatal(err)
			}
			fullOut, shortOut := client.AgentReviewOperation(got), client.AgentReviewOperationStatus(short)
			if !reflect.DeepEqual(got, full) || !reflect.DeepEqual(short, compact) || !reflect.DeepEqual(fullOut.ReviewOperation, full) || !reflect.DeepEqual(shortOut.ReviewOperationStatus, compact) || fullOut.Diagnostic == nil || !reflect.DeepEqual(fullOut.Diagnostic, shortOut.Diagnostic) {
				t.Fatal("diagnostics changed original facts or disagreed")
			}
			if !strings.Contains(fullOut.Diagnostic.Explanation, "No review trigger was posted") || !strings.Contains(fullOut.Diagnostic.NextAction, "no human confirmation") || !strings.Contains(fullOut.Diagnostic.NextAction, "No retry deadline") || reads != 2 {
				t.Fatal("diagnostic invented prerequisites, timing or extra reads", fullOut.Diagnostic, reads)
			}
			// A new finite reason does not weaken binding, known fields or privacy.
			full.HostedQuery.QueryURL = "https://github.com/owner/repo/pull/2?token=PRIVATE#issuecomment-123"
			_, err = c.ReviewOperation(t.Context(), full.ID)
			checkError(t, err, "invalid_response")
			full.HostedQuery.QueryURL = "https://github.com/owner/repo/pull/2#issuecomment-123"
			full.Reason = "hosted_allowance_PRIVATE_unknown"
			_, err = c.ReviewOperation(t.Context(), full.ID)
			checkError(t, err, "invalid_response")
			for _, edit := range []func(*client.ReviewOperationStatus){func(v *client.ReviewOperationStatus) { v.Channel = "cli" }, func(v *client.ReviewOperationStatus) { v.State = "uncertain" }, func(v *client.ReviewOperationStatus) { v.Reason = "hosted_allowance_unknown" }} {
				other := compact
				edit(&other)
				out := client.AgentReviewOperationStatus(other)
				if out.Diagnostic != nil && strings.Contains(out.Diagnostic.Explanation, "at this preparation guard") {
					t.Fatal("new preparation cause inferred for unrelated state")
				}
			}
		})
	}
}
