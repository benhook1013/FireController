package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestQueuedSchedulingHintIsCurrentPrivateBoundedMetadata(t *testing.T) {
	for _, mode := range []string{"compact", "full", "receipt", "selection"} {
		for _, cause := range []string{"operation_paused", "project_paused", "cooldown", "queue_order", "reservation_busy", "awaiting_scheduler", "PRIVATE_UNKNOWN", ""} {
			t.Run(mode+"_"+cause, func(t *testing.T) {
				full := sampleReviewOperation()
				full.State = "queued"
				full.Reason = ""
				full.AttemptID = ""
				full.Revision = 1
				full.WorkerID = "worker-one"
				state := selectionSample()
				state.Selection.ConfigurationRevision = full.ConfigurationRevision
				state.Operation = &full
				compact := client.ReviewOperationStatus{ID: full.ID, PullNumber: full.PullNumber, Channel: full.Channel, State: "queued", Stage: "queued", NextAction: "wait", Revision: 1, UpdatedAt: full.UpdatedAt}
				reads := 0
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					reads++
					if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Worker") != "worker-one" {
						t.Error("diagnostic changed binding")
					}
					w.Header().Set("X-FC-Review-Queued", cause)
					switch mode {
					case "compact":
						write(w, 200, compact)
					case "full":
						write(w, 200, full)
					case "receipt":
						write(w, 202, client.ReviewRequestReceipt{ID: full.ID, State: "queued", Revision: 1})
					case "selection":
						write(w, 200, state)
					}
				}))
				defer service.Close()
				c := newClient(t, service)
				var out any
				var err error
				switch mode {
				case "compact":
					v, e := c.ReviewOperationStatus(t.Context(), full.ID)
					err = e
					out = client.AgentReviewOperationStatus(v)
					if v.NextAction != "wait" {
						t.Fatal("hint changed next action")
					}
				case "full":
					v, e := c.ReviewOperation(t.Context(), full.ID)
					err = e
					out = client.AgentReviewOperation(v)
				case "receipt":
					v, e := c.RequestReview(t.Context(), "same-queued-key", client.ReviewRequest{ExpectedConfigurationRevision: full.ConfigurationRevision, PullNumber: full.PullNumber, Channel: full.Channel, HeadSHA: full.HeadSHA}, 0)
					err = e
					out = client.AgentReviewRequestResult(v)
				case "selection":
					v, e := c.WaitReviewSelection(t.Context(), state.Selection.ID, 0)
					err = e
					out = client.AgentReviewSelection(v)
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(out)
				if strings.Contains(string(raw), "queuedCause") || strings.Contains(string(raw), "PRIVATE_UNKNOWN") || strings.Contains(string(raw), "X-FC-Review-Queued") || reads != 1 {
					t.Fatal("raw/unbounded header escaped or caused more work")
				}
				hasDiagnostic := strings.Contains(string(raw), `"diagnostic"`)
				if hasDiagnostic != (cause != "" && cause != "PRIVATE_UNKNOWN") {
					t.Fatal("recognized/unknown scheduling cause changed")
				}
				if cause == "reservation_busy" && (!strings.Contains(string(raw), "reservation is occupied") || !strings.Contains(string(raw), "does not release")) {
					t.Fatal("past timer/capacity boundary unexplained")
				}
			})
		}
	}
}

func TestQueuedSchedulingHintPreservesPauseAndDrainPriority(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit_pause", true: "deployment_drain"}[draining], func(t *testing.T) {
			full := sampleReviewOperation()
			full.State = "queued"
			full.Reason = ""
			full.AttemptID = ""
			full.Revision = 1
			compact := client.ReviewOperationStatus{ID: full.ID, PullNumber: full.PullNumber, Channel: full.Channel, State: "queued", Stage: "queued", NextAction: "wait", Revision: 1, UpdatedAt: full.UpdatedAt, Wait: &client.ReviewOperationWait{Paused: true}}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("X-FC-Review-Queued", "reservation_busy")
				if draining {
					w.Header().Set("X-FC-Deployment-Draining", "true")
				}
				write(w, 200, compact)
			}))
			defer s.Close()
			got, err := newClient(t, s).ReviewOperationStatus(t.Context(), full.ID)
			if err != nil {
				t.Fatal(err)
			}
			out := client.AgentReviewOperationStatus(got)
			want := "paused before provider dispatch"
			wantAction := "wait"
			if draining {
				want = "draining new dispatch"
				wantAction = "wait_for_deployment"
			}
			if out.NextAction != wantAction || out.Diagnostic == nil || !strings.Contains(out.Diagnostic.Explanation, want) {
				t.Fatal("pause/drain priority changed", out)
			}
			// The selected-child envelope must retain the same drain precedence.
			state := selectionSample()
			full.ConfigurationRevision = state.Selection.ConfigurationRevision
			full.WorkerID = state.Selection.WorkerID
			full.Wait = compact.Wait
			state.Operation = &full
			selectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("X-FC-Review-Queued", "reservation_busy")
				if draining {
					w.Header().Set("X-FC-Deployment-Draining", "true")
				}
				write(w, 200, state)
			}))
			defer selectedServer.Close()
			selected, err := newClient(t, selectedServer).WaitReviewSelection(t.Context(), state.Selection.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			selectedOut := client.AgentReviewSelection(selected)
			if selectedOut.Operation == nil || selectedOut.Operation.Diagnostic == nil || !strings.Contains(selectedOut.Operation.Diagnostic.Explanation, want) {
				t.Fatal("selected child lost pause/drain priority")
			}

		})
	}
}
