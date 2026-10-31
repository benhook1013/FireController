package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewOperationStatusCLIWaitsForScheduledObservation(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "observation-guidance")
	worker := f.worker(t, project, "Builder")
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "contact_controller_owner"}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+project+"/reviews/operations/"+id+"/status" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != worker {
			t.Error("status guidance changed read semantics")
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "operation", id)
	want := status
	want.NextAction = "wait_for_observation"
	var got client.ReviewOperationStatus
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got != want || requests.Load() != 2 {
		t.Fatal("scheduled observation escalated or original facts changed", code, out, stderr, requests.Load())
	}
}

func TestReviewOperationHeldGuidanceCLICompactAndDiagnostics(t *testing.T) {
	for _, fixture := range []string{"parent_moved", "selected_parent_moved", "predecessor_parent_moved", "parent_unavailable", "hosted_allowance_cooldown", "target_changed"} {
		t.Run(fixture, func(t *testing.T) {
			reason := fixture
			if reason == "selected_parent_moved" {
				reason = "parent_moved"
			}
			f := setupServiceFixture(t)
			project := f.project(t, "parent-guidance")
			worker := f.worker(t, project, "Builder")
			id := "review_operation_" + strings.Repeat("a", 32)
			status := client.ReviewOperationStatus{ID: id, PullNumber: 3300, Channel: "hosted", State: "held", Stage: "held", Reason: reason, AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "read_diagnostics"}
			full := client.ReviewOperation{ID: id, ProjectID: project, ActorID: "principal_original", WorkerID: worker, ConfigurationRevision: 12, PolicyRevision: 2, CapabilityFingerprint: strings.Repeat("c", 64), PullNumber: 3300, Channel: "hosted", HeadSHA: strings.Repeat("d", 40), State: "held", Revision: 5, Reason: reason, AttemptID: status.AttemptID, CreatedAt: status.UpdatedAt, UpdatedAt: status.UpdatedAt}
			if fixture == "selected_parent_moved" {
				status.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: status.PullNumber, ParentPullNumber: 3299, ObservedBaseSHA: strings.Repeat("e", 40), ExpectedParentSHA: strings.Repeat("f", 40)}
				full.SourceRefusal = status.SourceRefusal
			}
			if reason == "predecessor_parent_moved" {
				status.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 3298, ParentPullNumber: 3297, ObservedBaseSHA: strings.Repeat("e", 40), ExpectedParentSHA: strings.Repeat("f", 40)}
				full.SourceRefusal = status.SourceRefusal
			}
			if reason == "parent_unavailable" {
				status.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 3298, BaseRef: "codex/former-parent"}
				full.SourceRefusal = status.SourceRefusal
			}
			wantStatus := status
			if reason == "target_changed" {
				wantStatus.NextAction = "qualify_current_target"
			}
			if reason == "hosted_allowance_cooldown" {
				status.RetryNotBefore = "2026-10-24T00:10:00Z"
				wantStatus = status
				wantStatus.NextAction = "new_request_after_cooldown"
				full.Allowance = &client.ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: status.UpdatedAt, RetryNotBefore: status.RetryNotBefore}
			}
			var compactReads, fullReads, unexpected atomic.Int64
			base := "/api/v1/projects/" + project + "/reviews/operations/" + id
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" && r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
					return
				}
				if r.Method != "GET" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != worker {
					t.Error("diagnostic changed the read or worker binding")
				}
				switch r.URL.Path {
				case base + "/status":
					if r.Header.Get("X-FC-Source-Refusal") != "v4" {
						t.Error("compact read did not negotiate source-refusal v4")
					}
					compactReads.Add(1)
					fields := map[string]any{}
					data, _ := json.Marshal(status)
					_ = json.Unmarshal(data, &fields)
					fields["future_metadata"] = map[string]any{"private": "PRIVATE_IGNORED_METADATA"}
					_ = json.NewEncoder(w).Encode(fields)
				case base:
					if r.Header.Get("X-FC-Source-Refusal") != "v4" || r.Header.Get("X-FC-Operation-Evidence") != "true" {
						t.Error("full read lost source-refusal v4 negotiation or original evidence opt-in")
					}
					fullReads.Add(1)
					_ = json.NewEncoder(w).Encode(full)
				default:
					unexpected.Add(1)
					w.WriteHeader(500)
				}
			}))
			defer service.Close()
			profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
			code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "operation", id)
			if strings.Contains(out, "PRIVATE_IGNORED_METADATA") {
				t.Fatal("unknown response metadata escaped CLI output")
			}
			var compactOut client.ReviewOperationStatusOutput
			if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &compactOut) != nil || !reflect.DeepEqual(compactOut.ReviewOperationStatus, wantStatus) || compactOut.Diagnostic == nil {
				t.Fatal("compact CLI parent guidance missing or changed retained facts", code, out, stderr)
			}
			code, out, stderr = mutationInvoke(t, profiles, "builder", "reviews", "operation", id, "--diagnostics")
			var fullOut client.ReviewOperationOutput
			if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &fullOut) != nil || !reflect.DeepEqual(fullOut.ReviewOperation, full) || !reflect.DeepEqual(fullOut.Diagnostic, compactOut.Diagnostic) {
				t.Fatal("full CLI parent guidance missing, changed retained facts or disagreed", code, out, stderr)
			}
			if fixture == "selected_parent_moved" {
				for _, hint := range []*client.ReviewOperationDiagnostic{compactOut.Diagnostic, fullOut.Diagnostic} {
					for _, fact := range []string{"selected PR #3300", "parent PR #3299", strings.Repeat("e", 40), strings.Repeat("f", 40), "not a current observation"} {
						if !strings.Contains(hint.Explanation, fact) {
							t.Fatal("selected parent diagnostic lost original comparison", fact, hint)
						}
					}
				}
			}
			if fixture == "parent_moved" {
				if compactOut.SourceRefusal != nil || fullOut.SourceRefusal != nil || !strings.Contains(compactOut.Diagnostic.Explanation, "does not identify the affected comparison") || strings.Contains(compactOut.Diagnostic.Explanation, "#3300") || strings.Contains(compactOut.Diagnostic.Explanation, "#3299") {
					t.Fatal("legacy nil metadata invented original comparison")
				}
			}
			if compactReads.Load() != 1 || fullReads.Load() != 1 || unexpected.Load() != 0 {
				t.Fatal("diagnostic polled, mutated or contacted another endpoint")
			}
		})
	}
}
