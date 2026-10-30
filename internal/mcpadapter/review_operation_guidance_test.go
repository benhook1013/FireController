package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewOperationStatusMCPWaitsForScheduledObservation(t *testing.T) {
	binding := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "contact_controller_owner"}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/operations/"+id+"/status" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			t.Error("SDK status guidance changed read semantics")
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var got client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id}), &got)
	want := status
	want.NextAction = "wait_for_observation"
	if got != want || requests.Load() != 2 {
		t.Fatal("SDK scheduled observation escalated or original facts changed", got, requests.Load())
	}
}

func TestReviewOperationHeldGuidanceMCPCompactAndDiagnostics(t *testing.T) {
	for _, reason := range []string{"parent_moved", "predecessor_parent_moved", "parent_unavailable", "hosted_allowance_cooldown"} {
		t.Run(reason, func(t *testing.T) {
			binding := localContext()
			id := "review_operation_" + strings.Repeat("a", 32)
			status := client.ReviewOperationStatus{ID: id, PullNumber: 3300, Channel: "hosted", State: "held", Stage: "held", Reason: reason, AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "read_diagnostics"}
			full := client.ReviewOperation{ID: id, ProjectID: binding.ProjectID, ActorID: "principal_original", WorkerID: binding.WorkerID, ConfigurationRevision: 12, PolicyRevision: 2, CapabilityFingerprint: strings.Repeat("c", 64), PullNumber: 3300, Channel: "hosted", HeadSHA: strings.Repeat("d", 40), State: "held", Revision: 5, Reason: reason, AttemptID: status.AttemptID, CreatedAt: status.UpdatedAt, UpdatedAt: status.UpdatedAt}
			if reason == "predecessor_parent_moved" {
				status.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 3298, ParentPullNumber: 3297, ObservedBaseSHA: strings.Repeat("e", 40), ExpectedParentSHA: strings.Repeat("f", 40)}
				full.SourceRefusal = status.SourceRefusal
			}
			if reason == "parent_unavailable" {
				status.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 3298, BaseRef: "codex/former-parent"}
				full.SourceRefusal = status.SourceRefusal
			}
			wantStatus := status
			if reason == "hosted_allowance_cooldown" {
				status.RetryNotBefore = "2026-10-24T00:10:00Z"
				wantStatus = status
				wantStatus.NextAction = "new_request_after_cooldown"
				full.Allowance = &client.ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: status.UpdatedAt, RetryNotBefore: status.RetryNotBefore}
			}
			var compactReads, fullReads, unexpected atomic.Int64
			base := "/api/v1/projects/" + binding.ProjectID + "/reviews/operations/" + id
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" && r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
					return
				}
				if r.Method != "GET" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
					t.Error("SDK diagnostic changed the read or worker binding")
				}
				switch r.URL.Path {
				case base + "/status":
					if r.Header.Get("X-FC-Source-Refusal") != "v2" {
						t.Error("compact source refusal metadata was not requested with v2 negotiation")
					}
					compactReads.Add(1)
					fields := map[string]any{}
					data, _ := json.Marshal(status)
					_ = json.Unmarshal(data, &fields)
					fields["future_metadata"] = map[string]any{"private": "PRIVATE_IGNORED_METADATA"}
					_ = json.NewEncoder(w).Encode(fields)
				case base:
					if r.Header.Get("X-FC-Source-Refusal") != "v2" || r.Header.Get("X-FC-Operation-Evidence") != "true" {
						t.Error("full source refusal v2 metadata or original operation evidence was not requested")
					}
					fullReads.Add(1)
					_ = json.NewEncoder(w).Encode(full)
				default:
					unexpected.Add(1)
					w.WriteHeader(500)
				}
			}))
			defer service.Close()
			binding.ServerURL = service.URL
			session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
			var compactOut client.ReviewOperationStatusOutput
			result := call(t, session, "review_operation_read", map[string]any{"operation_id": id})
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), "PRIVATE_IGNORED_METADATA") {
				t.Fatal("unknown response metadata escaped MCP output")
			}
			decodeResult(t, result, &compactOut)
			if !reflect.DeepEqual(compactOut.ReviewOperationStatus, wantStatus) || compactOut.Diagnostic == nil {
				t.Fatal("compact MCP parent guidance missing or changed retained facts", compactOut)
			}
			var fullOut client.ReviewOperationOutput
			decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "diagnostics": true}), &fullOut)
			if !reflect.DeepEqual(fullOut.ReviewOperation, full) || !reflect.DeepEqual(fullOut.Diagnostic, compactOut.Diagnostic) {
				t.Fatal("full MCP parent guidance missing, changed retained facts or disagreed", fullOut)
			}
			if compactReads.Load() != 1 || fullReads.Load() != 1 || unexpected.Load() != 0 {
				t.Fatal("SDK diagnostic polled, mutated or contacted another endpoint")
			}
		})
	}
}
