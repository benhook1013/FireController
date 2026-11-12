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

func TestCIWatchWorkflowRefusalsExplainInputRecovery(t *testing.T) {
	for _, test := range []struct {
		code     string
		status   int
		guidance string
	}{
		{"ci_watch_workflow_failures_unsupported", 409, "ordinary required-CI"},
		{"ci_watch_workflow_capacity", 503, "workflow selections"},
	} {
		t.Run(test.code, func(t *testing.T) {
			pins := ciWatchPins()
			pins[0].FailureWorkflowIDs = []int64{31}
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				var request struct {
					Targets []client.CIWatchTarget `json:"targets"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || !reflect.DeepEqual(request.Targets, pins) {
					t.Error("workflow refusal read changed original target pins")
				}
				write(w, test.status, map[string]any{"error": map[string]any{"code": test.code, "message": "private provider text"}, "request_id": "workflow-refusal"})
			}))
			defer server.Close()
			_, err := newClient(t, server).WatchCI(t.Context(), pins, 0)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Status != test.status || failure.RequestID != "workflow-refusal" || posts.Load() != 1 {
				t.Fatal("refusal changed identity or retried", err, posts.Load())
			}
			details, ok := failure.Details.(map[string]any)
			if !ok || !reflect.DeepEqual(details["targets"], pins) {
				t.Fatal("workflow refusal omitted the original target pins", failure.Details)
			}
			if !strings.Contains(failure.Action, test.guidance) || !strings.Contains(failure.Action, "PR/head/base pins") || !strings.Contains(failure.Action, "No automatic retry") {
				t.Fatal("missing safe input-specific guidance or original pins", failure)
			}
		})
	}
}

func TestCIWatchHeadSelectionAlwaysUsesPinnedHead(t *testing.T) {
	for _, state := range []string{"pending", "passed", "failed", "unconfigured", "closed", "superseded"} {
		for _, foreign := range []bool{false, true} {
			name := state + "/pinned"
			if foreign {
				name = state + "/foreign"
			}
			t.Run(name, func(t *testing.T) {
				pin := ciWatchPins()[0]
				item := client.CIWatchTargetResult{PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: state, BaseRef: "feature/parent", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", Selection: "head", CICommitSHA: pin.HeadSHA, Checks: []client.ReviewCIContext{}}
				switch state {
				case "pending":
					item.Required = 1
					item.Pending = 1
					item.Reason = "ci_required_checks_pending"
					item.Checks = []client.ReviewCIContext{{Context: "build", State: "pending", Reason: "ci_status_pending"}}
				case "passed":
					item.Required = 1
					item.Satisfied = 1
					item.Checks = []client.ReviewCIContext{{Context: "build", State: "satisfied"}}
				case "failed":
					item.Required = 1
					item.Failed = 1
					item.Reason = "ci_required_checks_failed"
					item.Checks = []client.ReviewCIContext{{Context: "build", State: "failed", Reason: "ci_failed"}}
				}
				if foreign {
					item.CICommitSHA = strings.Repeat("f", 40)
				}
				value := client.CIWatchResult{ProjectID: "project-one", State: state, TimedOut: state == "pending", Targets: []client.CIWatchTargetResult{item}}
				var posts atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-FC-CI-Watch", "change-v3")
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					posts.Add(1)
					write(w, 200, value)
				}))
				defer server.Close()
				got, err := newClient(t, server).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
				if foreign {
					checkError(t, err, "invalid_response")
				} else if err != nil || !reflect.DeepEqual(got, value) {
					t.Fatal("valid pinned head rejected", got, err)
				}
				if posts.Load() != 1 {
					t.Fatal("read retried", posts.Load())
				}
			})
		}
	}
}
