package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewAllowanceMCPStdioRoundTrip(t *testing.T) {
	for _, channel := range []string{"cli", "hosted", "legacy"} {
		t.Run(channel, func(t *testing.T) {
			binding := localContext()
			op := client.ReviewOperation{ID: "review_operation_" + strings.Repeat("a", 32), ProjectID: binding.ProjectID, ActorID: "principal_one", WorkerID: binding.WorkerID, ConfigurationRevision: 1, PolicyRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: "held", Revision: 3, Reason: "allowance_exhausted", AttemptID: "review_attempt_" + strings.Repeat("b", 32), CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
			zero, limit, window := int64(0), int64(3), int64(86400000)
			if channel == "cli" {
				op.Allowance = &client.ReviewAllowanceObservation{Kind: "cli_exhausted", ObservedAt: op.UpdatedAt, Remaining: &zero, Limit: &limit, FullCapacityInMS: &zero, RollingWindowMS: &window}
			}
			if channel == "hosted" {
				op.Channel = channel
				op.Reason = "hosted_allowance_cooldown"
				op.Allowance = &client.ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: op.UpdatedAt, RetryNotBefore: "2026-01-01T00:10:00Z"}
			}
			id := "review_selection_" + strings.Repeat("a", 32)
			selection := client.ReviewRunState{Selection: client.ReviewRunSelection{ID: id, ProjectID: op.ProjectID, ActorID: op.ActorID, WorkerID: op.WorkerID, Channel: op.Channel, ConfigurationRevision: 1, PolicyRevision: 1, ExpectedPullNumber: 2, State: "selected", Revision: 3, OperationID: op.ID, CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt}, Operation: &op}
			var calls atomic.Int64
			var invalid atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" {
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
					return
				}
				calls.Add(1)
				if r.Header.Get("X-FC-Worker") != binding.WorkerID || r.Header.Get("X-FC-Instance") != binding.InstanceID {
					t.Error("MCP allowance lost binding")
				}
				if invalid.Load() {
					raw, _ := json.Marshal(op)
					_, _ = w.Write([]byte(strings.Replace(string(raw), `"state":"held"`, `"state":"held","private":"PRIVATE_SENTINEL"`, 1)))
					return
				}
				if strings.Contains(r.URL.Path, "/selections") {
					_ = json.NewEncoder(w).Encode(selection)
				} else {
					_ = json.NewEncoder(w).Encode(op)
				}
			}))
			defer srv.Close()
			binding.ServerURL = srv.URL
			credential := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(credential, []byte("synthetic-retained-allowance-test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential), "chosen")
			expected, _ := json.Marshal(op)
			var want map[string]any
			_ = json.Unmarshal(expected, &want)
			for _, tool := range []struct {
				name string
				args map[string]any
			}{{"review_operation_read", map[string]any{"operation_id": op.ID, "diagnostics": true}}, {"review_selection_read", map[string]any{"selection_id": id}}, {"review_request", map[string]any{"idempotency_key": "retained-request", "expected_configuration_revision": 1, "pull_number": 2, "channel": op.Channel, "head_sha": op.HeadSHA}}, {"review_start", map[string]any{"idempotency_key": "retained-selection", "channel": op.Channel, "expected_pull_number": 2}}} {
				var got map[string]any
				decodeResult(t, call(t, session, tool.name, tool.args), &got)
				if nested, ok := got["operation"].(map[string]any); ok {
					got = nested
				}
				if channel == "hosted" {
					diagnostic, ok := got["diagnostic"].(map[string]any)
					if !ok || !strings.Contains(diagnostic["explanation"].(string), "terminal") || !strings.Contains(diagnostic["next_action"].(string), "fresh idempotency key") || !strings.Contains(diagnostic["next_action"].(string), "retry_not_before") {
						t.Fatal("cooldown retry requires owner clarification", got)
					}
					delete(got, "diagnostic")
				} else {
					diagnostic, ok := got["diagnostic"].(map[string]any)
					if !ok || !strings.Contains(diagnostic["explanation"].(string), "provider quota") || !strings.Contains(diagnostic["next_action"].(string), "No reset timestamp") || !strings.Contains(diagnostic["next_action"].(string), "absent timing means unknown") {
						t.Fatal("CLI quota was confused with local rounds or invented timing", got)
					}
					delete(got, "diagnostic")
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatal("MCP allowance changed or zero omitted", got)
				}
			}
			if calls.Load() != 4 {
				t.Fatal("MCP polled retained state", calls.Load())
			}
			invalid.Store(true)
			errorResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": op.ID, "diagnostics": true}), "invalid_response", "PRIVATE_SENTINEL")
			if calls.Load() != 5 {
				t.Fatal("MCP retried malformed read")
			}
		})
	}
}
