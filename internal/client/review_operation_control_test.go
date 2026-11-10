package client_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewOperationWaitNegotiatedFullCompactAndLegacy(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			for _, legacy := range []bool{false, true} {
				t.Run(strings.Join([]string{boolName(compact), boolName(paused), boolName(legacy)}, "/"), func(t *testing.T) {
					op := sampleReviewOperation()
					op.Channel = "hosted"
					op.State = "queued"
					op.AttemptID = ""
					op.Reason = ""
					wait := &client.ReviewOperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-01-01T00:10:00Z", Paused: paused}
					if !legacy {
						op.Wait = wait
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							handshake(w)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if r.Header.Get("X-FC-Operation-Wait") != "3" {
							t.Error("wait extension not negotiated")
						}
						if compact {
							_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: "queued", Reason: "", AttemptID: "", UpdatedAt: op.UpdatedAt, NextAction: "wait", Wait: op.Wait})
						} else {
							_ = json.NewEncoder(w).Encode(op)
						}
					}))
					defer server.Close()
					c := newClient(t, server)
					if compact {
						v, err := c.ReviewOperationStatus(t.Context(), op.ID)
						if err != nil || (v.Wait == nil) != legacy || v.Wait != nil && *v.Wait != *wait {
							t.Fatal(v, err)
						}
					} else {
						v, err := c.ReviewOperation(t.Context(), op.ID)
						if err != nil || (v.Wait == nil) != legacy || v.Wait != nil && *v.Wait != *wait {
							t.Fatal(v, err)
						}
					}
				})
			}
		}
	}
}
func boolName(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestReviewOperationControlBoundSingleShotAndMalformed(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	var requests atomic.Int64
	for _, action := range []string{"pause", "resume", "cancel"} {
		t.Run(action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				requests.Add(1)
				body, _ := io.ReadAll(r.Body)
				want := `{"expected_revision":3,"action":"` + action + `"}`
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+id+"/control" || r.Header.Get("Idempotency-Key") != "same-key" || r.Header.Get("X-FC-Worker") != "worker-one" || string(body) != want {
					t.Error("bound exact control changed", r.URL, string(body))
				}
				state := "queued"
				if action == "cancel" {
					state = "cancelled"
				}
				_ = json.NewEncoder(w).Encode(client.ReviewOperationControlReceipt{ID: id, State: state, Revision: 4, Paused: action == "pause", RetryNotBefore: "2026-01-01T00:10:00Z"})
			}))
			defer server.Close()
			c := newClient(t, server)
			v, err := c.ControlReviewOperation(t.Context(), "same-key", id, client.ReviewOperationControl{ExpectedRevision: 3, Action: action})
			if err != nil || v.Revision != 4 || v.ID != id {
				t.Fatal(v, err)
			}
			before := requests.Load()
			for _, bad := range []client.ReviewOperationControl{{ExpectedRevision: 0, Action: action}, {ExpectedRevision: 3, Action: "stop"}} {
				_, err := c.ControlReviewOperation(t.Context(), "same-key", id, bad)
				if err == nil {
					t.Fatal("bad control accepted")
				}
			}
			_, err = c.ControlReviewOperation(t.Context(), "same-key", "bad", client.ReviewOperationControl{ExpectedRevision: 3, Action: action})
			if err == nil || requests.Load() != before {
				t.Fatal("malformed contacted service")
			}
		})
	}
}

func TestReviewOperationWaitRejectsMalformedFacts(t *testing.T) {
	for _, bad := range []string{`null`, `{"paused":null}`, `{"paused":true,"retry_not_before":"2026-01-01T00:10:00Z"}`, `{"paused":false,"baseline_attempt_id":"review_attempt_` + strings.Repeat("a", 32) + `"}`, `{"paused":true,"private":"secret"}`, `{"paused":false,"paused":true}`} {
		t.Run(bad, func(t *testing.T) {
			op := sampleReviewOperation()
			op.Channel = "hosted"
			op.State = "queued"
			op.AttemptID = ""
			raw, _ := json.Marshal(op)
			raw = append(raw[:len(raw)-1], []byte(`,"wait":`+bad+`}`)...)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(raw)
			}))
			defer server.Close()
			_, err := newClient(t, server).ReviewOperation(t.Context(), op.ID)
			if err == nil {
				t.Fatal("malformed wait accepted")
			}
		})
	}
}

func TestReviewOperationControlActualWriterReadAndProjectIsolation(t *testing.T) {
	f, _ := executionFixture(t)
	for token, permissions := range map[string][]string{"synthetic-writer": {"read", "write"}, "synthetic-reader": {"read"}} {
		sum := sha256.Sum256([]byte(token))
		f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, f.sender, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions})
	}
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	writer := f.newClient(t, "synthetic-writer", f.sender, srv.URL)
	peer := f.newClient(t, "synthetic-writer", f.recipient, srv.URL)
	reader := f.newClient(t, "synthetic-reader", f.sender, srv.URL)
	queued, err := queuedReviewRequest(t, writer, t.Context(), "request", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.ControlReviewOperation(t.Context(), "reader-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 1, Action: "pause"})
	checkError(t, err, "forbidden")
	paused, err := peer.ControlReviewOperation(t.Context(), "peer-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 1, Action: "pause"})
	if err != nil || !paused.Paused {
		t.Fatal(paused, err)
	}
	after, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil || after.ActorID != before.ActorID || after.WorkerID != f.sender || after.Wait == nil || !after.Wait.Paused {
		t.Fatal(after, err)
	}
	foreign := mutationFixture{store: f.store, admin: f.admin, tokenFile: f.tokenFile}
	foreign.project = f.call(t, "POST", "/api/v1/projects", f.admin, "", map[string]any{"name": "Other", "repository": "other/repository"})["id"].(string)
	otherWorker := f.call(t, "POST", "/api/v1/projects/"+foreign.project+"/workers", f.admin, "", map[string]any{"name": "Other"})["id"].(string)
	_, err = foreign.newClient(t, f.admin, otherWorker, srv.URL).ControlReviewOperation(t.Context(), "foreign-pause", queued.ID, client.ReviewOperationControl{ExpectedRevision: 2, Action: "pause"})
	checkError(t, err, "not_found")
	_, err = peer.ControlReviewOperation(t.Context(), "cancel", queued.ID, client.ReviewOperationControl{ExpectedRevision: 2, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = peer.ControlReviewOperation(t.Context(), "cannot-resume", queued.ID, client.ReviewOperationControl{ExpectedRevision: 3, Action: "resume"})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "review_operation_started" || !strings.Contains(failure.Action, "No provider cancellation occurred") || !strings.Contains(failure.Action, "held target_changed") || !strings.Contains(failure.Action, "Do not submit a replacement for posting, active or uncertain work") {
		t.Fatal(err)
	}
	done, err := reader.ReviewOperation(t.Context(), queued.ID)
	if err != nil || done.State != "cancelled" || done.Wait != nil {
		t.Fatal(done, err)
	}
}

func TestReviewOperationRetainedOverrideFlagsWire(t *testing.T) {
	for _, route := range []string{"read", "terminal-request"} {
		for _, test := range []struct {
			name, fields           string
			valid, allow, override bool
		}{
			{"legacy", "", true, false, false},
			{"both", `,"allow_out_of_order":true,"override_file_limit":true`, true, true, true},
			{"allow", `,"allow_out_of_order":true`, true, true, false},
			{"override", `,"override_file_limit":true`, true, false, true},
			{"false", `,"allow_out_of_order":false,"override_file_limit":false`, true, false, false},
			{"null-allow", `,"allow_out_of_order":null`, false, false, false},
			{"null-override", `,"override_file_limit":null`, false, false, false},
			{"string-allow", `,"allow_out_of_order":"true"`, false, false, false},
			{"number-override", `,"override_file_limit":1`, false, false, false},
			{"alias", `,"Allow_Out_Of_Order":true`, false, false, false},
			{"duplicate", `,"allow_out_of_order":true,"allow_out_of_order":false`, false, false, false},
		} {
			t.Run(route+"/"+test.name, func(t *testing.T) {
				operation := sampleReviewOperation()
				operation.WorkerID, operation.State = "worker-one", "completed"
				encoded, err := json.Marshal(operation)
				if err != nil {
					t.Fatal(err)
				}
				body := strings.TrimSuffix(string(encoded), "}") + test.fields + "}"
				var posts atomic.Int64
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					if r.Method == "POST" {
						posts.Add(1)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				defer service.Close()
				c := newClient(t, service)
				defer c.Close()
				var got client.ReviewOperation
				if route == "read" {
					got, err = c.ReviewOperation(t.Context(), executionOperationID)
				} else {
					result, e := c.RequestReview(t.Context(), "retained-flags", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: operation.HeadSHA}, 0)
					err = e
					if result.Operation != nil {
						got = *result.Operation
					}
				}
				if !test.valid {
					checkError(t, err, "invalid_response")
					var failure *client.Error
					if !errors.As(err, &failure) || failure.OutcomeUnknown != (route == "terminal-request") {
						t.Fatal("lost outcome semantics", err)
					}
				} else {
					if err != nil {
						t.Fatal("valid retained operation rejected", err)
					}
					b, e := json.Marshal(got)
					if e != nil {
						t.Fatal(e)
					}
					var fields map[string]any
					if json.Unmarshal(b, &fields) != nil || (fields["allow_out_of_order"] == true) != test.allow || (fields["override_file_limit"] == true) != test.override {
						t.Fatal("retained flags changed", string(b))
					}
				}
				wantPosts := int64(0)
				if route == "terminal-request" {
					wantPosts = 1
				}
				if posts.Load() != wantPosts {
					t.Fatal("request retried or read mutated", posts.Load())
				}
			})
		}
	}
}

func TestOperationWaitV3AllowanceWireAndLegacyReads(t *testing.T) {
	for _, kind := range []string{"queued-full", "queued-status", "held-cli-full", "held-hosted-full", "held-hosted-status", "held-cli-request", "held-hosted-request", "legacy-full", "legacy-status"} {
		t.Run(kind, func(t *testing.T) {
			op := sampleReviewOperation()
			op.WorkerID = "worker-one"
			compact := strings.HasSuffix(kind, "status")
			terminal := strings.HasSuffix(kind, "request")
			held := strings.HasPrefix(kind, "held-")
			legacy := strings.HasPrefix(kind, "legacy-")
			hosted := strings.Contains(kind, "hosted")
			op.Channel = "cli"
			if hosted {
				op.Channel = "hosted"
			}
			allowance := map[string]any{"kind": "cli_exhausted", "observed_at": "2026-01-01T00:00:00Z", "remaining": 0, "limit": 8, "full_capacity_in_ms": 0, "rolling_window_ms": 3600000}
			if hosted {
				allowance = map[string]any{"kind": "hosted_cooldown", "observed_at": "2026-01-01T00:00:00Z", "retry_not_before": "2026-01-01T00:10:00Z"}
			}
			if held {
				op.State = "held"
				op.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
				op.Reason = "allowance_exhausted"
				if hosted {
					op.Reason = "hosted_allowance_cooldown"
				}
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				requests.Add(1)
				version := r.Header.Get("X-FC-Operation-Wait")
				if version != "3" {
					t.Error("current client did not negotiate allowance version3", version)
				}
				var encoded []byte
				if compact {
					stage, next := "queued", "wait"
					if held {
						stage, next = "held", "read_diagnostics"
					}
					encoded, _ = json.Marshal(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: stage, Reason: op.Reason, AttemptID: op.AttemptID, UpdatedAt: op.UpdatedAt, NextAction: next})
				} else {
					encoded, _ = json.Marshal(op)
				}
				var wire map[string]any
				if json.Unmarshal(encoded, &wire) != nil {
					t.Error("fixture encoding")
					return
				}
				if !held {
					wait := map[string]any{"baseline_attempt_id": "review_attempt_" + strings.Repeat("b", 32), "retry_not_before": "2026-01-01T00:10:00Z", "paused": false}
					if version == "3" && !legacy {
						wait["cli_allowance"] = allowance
					}
					wire["wait"] = wait
				} else if version == "3" {
					if compact {
						wire["retry_not_before"] = allowance["retry_not_before"]
					} else {
						wire["allowance"] = allowance
					}
				}
				write(w, 200, wire)
			}))
			defer server.Close()
			c := newClient(t, server)
			defer c.Close()
			var value any
			var err error
			if compact {
				value, err = c.ReviewOperationStatus(t.Context(), op.ID)
			} else if terminal {
				result, e := c.RequestReview(t.Context(), "allowance-v3-original", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: op.Channel, HeadSHA: op.HeadSHA}, 0)
				value, err = result.Operation, e
			} else {
				value, err = c.ReviewOperation(t.Context(), op.ID)
			}
			if err != nil {
				t.Fatal("version3 operation facts rejected", err)
			}
			raw, e := json.Marshal(value)
			if e != nil {
				t.Fatal(e)
			}
			var got map[string]any
			if json.Unmarshal(raw, &got) != nil {
				t.Fatal("invalid operation output")
			}
			var facts any
			if held && compact {
				if got["retry_not_before"] != allowance["retry_not_before"] {
					t.Fatal("held cooldown deadline lost", string(raw))
				}
			} else {
				if held {
					facts = got["allowance"]
				} else {
					wait, ok := got["wait"].(map[string]any)
					if !ok {
						t.Fatal("original wait lost", string(raw))
					}
					facts = wait["cli_allowance"]
				}
				if legacy {
					if facts != nil {
						t.Fatal("legacy response invented allowance", facts)
					}
				} else {
					a, _ := json.Marshal(allowance)
					b, _ := json.Marshal(facts)
					if string(a) != string(b) {
						t.Fatal("retained allowance changed", string(a), string(b))
					}
				}
			}
			if requests.Load() != 1 {
				t.Fatal("read or original request retried", requests.Load())
			}
		})
	}
}

func TestOperationWaitV3RejectsMalformedQueuedAllowance(t *testing.T) {
	valid := `{"kind":"cli_exhausted","observed_at":"2026-01-01T00:00:00Z","remaining":0,"limit":8,"full_capacity_in_ms":0,"rolling_window_ms":3600000}`
	for _, compact := range []bool{false, true} {
		for name, allowance := range map[string]string{
			"hosted-scope": valid, "missing-baseline": valid, "null": `null`, "missing": `{}`, "positive": strings.Replace(valid, `"remaining":0`, `"remaining":1`, 1), "unknown-kind": strings.Replace(valid, `cli_exhausted`, `available`, 1), "unknown-field": strings.TrimSuffix(valid, "}") + `,"private":"secret"}`, "null-remaining": strings.Replace(valid, `"remaining":0`, `"remaining":null`, 1), "claimed-reset": strings.TrimSuffix(valid, "}") + `,"retry_not_before":"2026-01-01T00:10:00Z"}`,
		} {
			t.Run(boolName(compact)+"/"+name, func(t *testing.T) {
				op := sampleReviewOperation()
				if name == "hosted-scope" {
					op.Channel = "hosted"
				}
				var raw []byte
				if compact {
					raw, _ = json.Marshal(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: "queued", Revision: 1, Stage: "queued", UpdatedAt: op.UpdatedAt, NextAction: "wait"})
				} else {
					raw, _ = json.Marshal(op)
				}
				body := strings.TrimSuffix(string(raw), "}") + `,"wait":{"baseline_attempt_id":"review_attempt_` + strings.Repeat("b", 32) + `","retry_not_before":"2026-01-01T00:10:00Z","paused":false,"cli_allowance":` + allowance + `}}`
				if name == "missing-baseline" {
					body = strings.Replace(body, `"baseline_attempt_id":"review_attempt_`+strings.Repeat("b", 32)+`"`, `"baseline_attempt_id":""`, 1)
					body = strings.Replace(body, `"retry_not_before":"2026-01-01T00:10:00Z"`, `"retry_not_before":""`, 1)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				c := newClient(t, server)
				defer c.Close()
				var err error
				if compact {
					_, err = c.ReviewOperationStatus(t.Context(), op.ID)
				} else {
					_, err = c.ReviewOperation(t.Context(), op.ID)
				}
				checkError(t, err, "invalid_response")
			})
		}
	}
}
