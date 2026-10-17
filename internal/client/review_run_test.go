package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

const runSelectionID = "review_selection_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func selectionSample() client.ReviewRunState {
	op := sampleReviewOperation()
	op.WorkerID = "worker-one"
	op.State = "uncertain"
	op.ConfigurationRevision = 7
	return client.ReviewRunState{Selection: client.ReviewRunSelection{ID: runSelectionID, ProjectID: op.ProjectID, ActorID: op.ActorID, WorkerID: op.WorkerID, Channel: op.Channel, ConfigurationRevision: op.ConfigurationRevision, PolicyRevision: op.PolicyRevision, ExpectedPullNumber: 2, State: "selected", Revision: 3, OperationID: op.ID, CreatedAt: "2026-10-18T01:00:00Z", UpdatedAt: "2026-10-18T00:59:00Z"}, Operation: &op}
}
func selectionRequest() client.ReviewRunRequest {
	return client.ReviewRunRequest{Channel: "cli", ExpectedPullNumber: roundInt(2), ExpectedConfigurationRevision: roundInt(7)}
}

func TestReviewRunTerminalOrOriginalFlatReceipt(t *testing.T) {
	for _, state := range []string{"selected", "held", "empty", "receipt"} {
		t.Run(state, func(t *testing.T) {
			value := selectionSample()
			switch state {
			case "held", "empty":
				value.Selection.State = state
				value.Selection.OperationID = ""
				value.Operation = nil
				value.Selection.Reason = "queue_blocked"
				if state == "empty" {
					value.Selection.Reason = "queue_complete"
				}
			}
			receipt := client.ReviewRequestReceipt{ID: runSelectionID, State: "queued", Revision: 1}
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				posts.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/selections" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=900" || r.Header.Get("Idempotency-Key") != "original-selection" || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("selection lost binding")
				}
				var request client.ReviewRunRequest
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Channel != "cli" || request.ExpectedPullNumber == nil || *request.ExpectedPullNumber != 2 || request.ExpectedConfigurationRevision == nil || *request.ExpectedConfigurationRevision != 7 {
					t.Error("action identity changed")
				}
				if state == "receipt" {
					write(w, 202, receipt)
				} else {
					write(w, 200, value)
				}
			}))
			defer srv.Close()
			c := newClient(t, srv)
			got, err := c.RunReview(t.Context(), "original-selection", selectionRequest(), 900)
			if err != nil || posts.Load() != 1 {
				t.Fatal(got, err)
			}
			var want any = value
			if state == "receipt" {
				want = receipt
				if got.Receipt == nil || got.Result != nil {
					t.Fatal("wrapped receipt")
				}
			} else if got.Result == nil || got.Receipt != nil {
				t.Fatal("wrapped result")
			}
			raw, _ := json.Marshal(got)
			expected, _ := json.Marshal(want)
			if string(raw) != string(expected) {
				t.Fatal("wire union was not flat")
			}
		})
	}
	for _, invalid := range []client.ReviewRunResult{{}, {Receipt: &client.ReviewRequestReceipt{}, Result: &client.ReviewRunState{}}} {
		if _, err := json.Marshal(invalid); err == nil {
			t.Fatal("invalid union encoded")
		}
	}
}

func TestReviewRunRejectsUnboundOrNonterminalMutationResults(t *testing.T) {
	cases := map[string]func(*client.ReviewRunState){
		"unconfigured-policy": func(v *client.ReviewRunState) { v.Selection.PolicyRevision = 0; v.Operation.PolicyRevision = 0 },
		"project":             func(v *client.ReviewRunState) { v.Selection.ProjectID = "foreign" }, "worker": func(v *client.ReviewRunState) { v.Selection.WorkerID = "other"; v.Operation.WorkerID = "other" }, "channel": func(v *client.ReviewRunState) { v.Selection.Channel = "hosted"; v.Operation.Channel = "hosted" }, "config": func(v *client.ReviewRunState) {
			v.Selection.ConfigurationRevision = 8
			v.Operation.ConfigurationRevision = 8
		}, "assertion": func(v *client.ReviewRunState) { v.Selection.ExpectedPullNumber = 0 }, "actor-link": func(v *client.ReviewRunState) { v.Operation.ActorID = "other" }, "policy-link": func(v *client.ReviewRunState) { v.Operation.PolicyRevision++ }, "operation-link": func(v *client.ReviewRunState) {
			v.Selection.OperationID = "review_operation_" + strings.Repeat("b", 32)
		}, "child-target": func(v *client.ReviewRunState) { v.Operation.PullNumber = 3 }, "nonterminal-child": func(v *client.ReviewRunState) { v.Operation.State = "active" }, "nonterminal-selection": func(v *client.ReviewRunState) {
			v.Selection.State = "selecting"
			v.Selection.OperationID = ""
			v.Operation = nil
		}, "held-child": func(v *client.ReviewRunState) { v.Selection.State = "held"; v.Selection.Reason = "blocked" }, "bad-empty": func(v *client.ReviewRunState) {
			v.Selection.State = "empty"
			v.Selection.Reason = ""
			v.Selection.OperationID = ""
			v.Operation = nil
		}, "malformed-child": func(v *client.ReviewRunState) { v.Operation.HeadSHA = "bad" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			value := selectionSample()
			change(&value)
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				posts.Add(1)
				write(w, 200, value)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			_, err := c.RunReview(t.Context(), "key", selectionRequest(), 0)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown || posts.Load() != 1 {
				t.Fatal("invalid dispatched result lost uncertainty", err)
			}
		})
	}
	for _, raw := range []string{`{"selection":null}`, `{"id":"review_operation_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"queued","revision":1}`, `{"id":"` + runSelectionID + `","state":"selecting","revision":2}`, strings.Repeat(" ", 64<<10) + `{}`} {
		t.Run("shape", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(202)
				_, _ = io.WriteString(w, raw)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			_, err := c.RunReview(t.Context(), "key", selectionRequest(), 0)
			var failure *client.Error
			if !errors.As(err, &failure) || !failure.OutcomeUnknown {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewRunReadsPendingAndStrictOptionalFields(t *testing.T) {
	for _, mode := range []string{"queued", "selecting", "selected", "missing", "unknown", "null-child", "foreign", "wrong-id", "other-worker"} {
		t.Run(mode, func(t *testing.T) {
			value := selectionSample()
			value.Operation.State = "active"
			if mode == "queued" || mode == "selecting" {
				value.Selection.State = mode
				value.Selection.OperationID = ""
				value.Operation = nil
			}
			if mode == "other-worker" {
				value.Selection.WorkerID = "other"
				value.Operation.WorkerID = "other"
			}
			if mode == "foreign" {
				value.Selection.ProjectID = "other"
			}
			if mode == "wrong-id" {
				value.Selection.ID = "review_selection_" + strings.Repeat("b", 32)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/selections/"+runSelectionID || r.Header.Get("Prefer") != "wait=0" {
					t.Error("read polled or changed route")
				}
				raw, _ := json.Marshal(value)
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				if mode == "missing" {
					raw = []byte(`{}`)
				}
				if mode == "unknown" {
					fields["capability_fingerprint"] = json.RawMessage(`"private"`)
					raw, _ = json.Marshal(fields)
				}
				if mode == "null-child" {
					fields["operation"] = json.RawMessage(`null`)
					raw, _ = json.Marshal(fields)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			_, err := c.ReviewSelection(t.Context(), runSelectionID)
			if mode == "queued" || mode == "selecting" || mode == "selected" || mode == "other-worker" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var failure *client.Error
				if !errors.As(err, &failure) || failure.OutcomeUnknown || failure.Code != "invalid_response" {
					t.Fatal("read failure became mutation", err)
				}
			}
		})
	}
}

func TestReviewRunLocalValidationAndCancellation(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		for _, received := range []bool{false, true} {
			t.Run(map[bool]string{false: "read", true: "post"}[mutation]+map[bool]string{false: "-unsent", true: "-received"}[received], func(t *testing.T) {
				started := make(chan struct{})
				release := make(chan struct{})
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/instance") {
						handshake(w)
						return
					}
					calls.Add(1)
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer srv.Close()
				defer close(release)
				c := newClient(t, srv)
				if mutation {
					for _, request := range []client.ReviewRunRequest{{}, {Channel: "other"}, {Channel: "cli", ExpectedPullNumber: roundInt(0)}, {Channel: "cli", ExpectedConfigurationRevision: roundInt(0)}} {
						if _, err := c.RunReview(t.Context(), "key", request, 0); err == nil {
							t.Fatal("invalid request accepted")
						}
					}
					for _, wait := range []int{-1, 901} {
						if _, err := c.RunReview(t.Context(), "key", selectionRequest(), wait); err == nil {
							t.Fatal("invalid wait accepted")
						}
					}
				} else {
					if _, err := c.WaitReviewSelection(t.Context(), "../bad", 0); err == nil {
						t.Fatal("invalid id accepted")
					}
					if _, err := c.WaitReviewSelection(t.Context(), runSelectionID, 901); err == nil {
						t.Fatal("invalid read wait accepted")
					}
				}
				if calls.Load() != 0 {
					t.Fatal("invalid request used HTTP")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !received {
					cancel()
				}
				done := make(chan error, 1)
				go func() {
					if mutation {
						_, err := c.RunReview(ctx, "key", selectionRequest(), 900)
						done <- err
					} else {
						_, err := c.WaitReviewSelection(ctx, runSelectionID, 900)
						done <- err
					}
				}()
				if received {
					select {
					case <-started:
						cancel()
					case <-time.After(2 * time.Second):
						t.Fatal("request never received")
					}
				}
				select {
				case err := <-done:
					var failure *client.Error
					if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown != (mutation && received) {
						t.Fatal("cancellation scope wrong", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("wait failed to detach")
				}
			})
		}
	}
}

func TestReviewRunPostAcceptedFailureAndLostResponse(t *testing.T) {
	for _, mode := range []string{"lost", "revoked"} {
		for _, mutation := range []bool{true, false} {
			t.Run(mode+map[bool]string{true: "-post", false: "-read"}[mutation], func(t *testing.T) {
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/instance") {
						w.Header().Set("Connection", "close")
						handshake(w)
						return
					}
					calls.Add(1)
					if mode == "lost" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"access revoked","details":{"outcome_unknown":true}},"request_id":"req"}`)
				}))
				defer srv.Close()
				c := newClient(t, srv)
				var err error
				if mutation {
					_, err = c.RunReview(t.Context(), "key", selectionRequest(), 2)
				} else {
					_, err = c.WaitReviewSelection(t.Context(), runSelectionID, 2)
				}
				var failure *client.Error
				if !errors.As(err, &failure) || failure.OutcomeUnknown != mutation || calls.Load() != 1 {
					t.Fatal("lost response scope/retry wrong", err)
				}
			})
		}
	}
}

func TestReviewRunActualHTTPAcceptsOnceAndReadDoesNotSelect(t *testing.T) {
	f, _ := executionFixture(t)
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := f.newClient(t, f.admin, f.sender, srv.URL)
	request := client.ReviewRunRequest{Channel: "cli", ExpectedPullNumber: roundInt(2), ExpectedConfigurationRevision: roundInt(1)}
	before := executionCounts(t, f)
	provider := f.providerCalls.Load()
	first, err := c.RunReview(t.Context(), "native-selection-once", request, 0)
	if err != nil || first.Receipt == nil || first.Result != nil {
		t.Fatal("real selection acceptance failed", err)
	}
	var selected int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_selections`).Scan(&selected); err != nil || selected != 1 {
		t.Fatal("selection journal not singular", err)
	}
	after := executionCounts(t, f)
	if after[0] != before[0] {
		t.Fatal("acceptance dispatched native operation")
	}
	replay, err := c.RunReview(t.Context(), "native-selection-once", request, 0)
	if err != nil || replay.Receipt == nil || *first.Receipt != *replay.Receipt || executionCounts(t, f) != after {
		t.Fatal("replay changed selection effect", err)
	}
	current, err := c.ReviewSelection(t.Context(), first.Receipt.ID)
	if err != nil || current.Selection.ProjectID != f.project || current.Selection.WorkerID != f.sender || current.Selection.Channel != "cli" || current.Selection.ExpectedPullNumber != 2 || current.Selection.State != "queued" || current.Operation != nil || executionCounts(t, f) != after || posts.Load() != 2 || f.providerCalls.Load() != provider {
		t.Fatal("retained read selected or lost scope", err)
	}
}
