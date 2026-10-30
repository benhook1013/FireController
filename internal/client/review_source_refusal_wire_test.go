package client_test

import (
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMissingParentSourceRefusalWireNegotiatesV3(t *testing.T) {
	full := sampleReviewOperation()
	full.State, full.Reason, full.AttemptID = "held", "parent_unavailable", "review_attempt_"+strings.Repeat("b", 32)
	full.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 4, BaseRef: "codex/former-parent"}
	compact := client.ReviewOperationStatus{ID: full.ID, PullNumber: full.PullNumber, Channel: full.Channel, State: full.State, Reason: full.Reason, AttemptID: full.AttemptID, Stage: "held", NextAction: "read_diagnostics", Revision: full.Revision, UpdatedAt: full.UpdatedAt, SourceRefusal: full.SourceRefusal}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Header.Get("X-FC-Source-Refusal") != "v3" {
			t.Error("client did not negotiate missing-parent variant")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/status") {
			json.NewEncoder(w).Encode(compact)
		} else {
			json.NewEncoder(w).Encode(full)
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	a, err := c.ReviewOperation(t.Context(), full.ID)
	if err != nil || a.SourceRefusal == nil || a.SourceRefusal.BaseRef != full.SourceRefusal.BaseRef {
		t.Fatal(a, err)
	}
	b, err := c.ReviewOperationStatus(t.Context(), full.ID)
	if err != nil || b.SourceRefusal == nil || b.SourceRefusal.PullNumber != 4 {
		t.Fatal(b, err)
	}
}

func TestPredecessorSourceRefusalV3StrictWire(t *testing.T) {
	good := `{"pull_number":6,"cause":"discovery_incomplete"}`
	cases := []struct {
		name, raw string
		valid     bool
	}{
		{"legacy absent", "", true}, {"discovery", good, true},
		{"queue absent", strings.Replace(good, "discovery_incomplete", "not_in_queue", 1), true},
		{"incoming route", strings.Replace(good, "discovery_incomplete", "incoming_route_pending", 1), true},
		{"wrong reason", good, false},
		{"null", `null`, false}, {"unknown cause", strings.Replace(good, "discovery_incomplete", "private_unknown", 1), false},
		{"null cause", `{"pull_number":6,"cause":null}`, false},
		{"empty cause", `{"pull_number":6,"cause":""}`, false},
		{"selected PR", `{"pull_number":2,"cause":"not_in_queue"}`, false},
		{"alias", `{"pull_number":6,"Cause":"not_in_queue"}`, false},
		{"duplicate", `{"pull_number":6,"cause":"not_in_queue","cause":"discovery_incomplete"}`, false},
		{"private nested", `{"pull_number":6,"cause":"not_in_queue","capture":{"body":"PRIVATE_SENTINEL"}}`, false},
		{"hybrid parent", `{"pull_number":6,"cause":"not_in_queue","parent_pull_number":4}`, false},
		{"hybrid branch", `{"pull_number":6,"cause":"not_in_queue","base_ref":"private-branch"}`, false},
	}
	for _, compact := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name+map[bool]string{false: "_full", true: "_compact"}[compact], func(t *testing.T) {
				operation := sampleReviewOperation()
				operation.State = "held"
				operation.Reason = "predecessor_unfinished"
				if tc.name == "wrong reason" {
					operation.Reason = "parent_unavailable"
				}
				operation.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
				var value any = operation
				if compact {
					value = client.ReviewOperationStatus{ID: operation.ID, PullNumber: 2, Channel: operation.Channel, State: operation.State, Reason: operation.Reason, AttemptID: operation.AttemptID, Revision: operation.Revision, UpdatedAt: operation.UpdatedAt, Stage: "held", NextAction: "read_diagnostics"}
				}
				raw, _ := json.Marshal(value)
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				if tc.raw != "" {
					fields["source_refusal"] = json.RawMessage(tc.raw)
				}
				raw, _ = json.Marshal(fields)
				reads := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						if r.Header.Get("X-FC-Source-Refusal") != "" {
							t.Error("refusal optin leaked to identity")
						}
						handshake(w)
						return
					}
					reads++
					want := "/api/v1/projects/project-one/reviews/operations/" + operation.ID
					if compact {
						want += "/status"
					}
					if r.Method != "GET" || r.URL.Path != want || r.Header.Get("X-FC-Source-Refusal") != "v3" || r.Header.Get("Authorization") != "Bearer "+testToken {
						t.Error("source refusal read negotiation/scope changed")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(raw)
				}))
				defer server.Close()
				c := newClient(t, server)
				var err error
				var refusal *client.ReviewSourceRefusal
				if compact {
					v, e := c.ReviewOperationStatus(t.Context(), operation.ID)
					err = e
					refusal = v.SourceRefusal
				} else {
					v, e := c.ReviewOperation(t.Context(), operation.ID)
					err = e
					refusal = v.SourceRefusal
				}
				if tc.valid {
					if err != nil || (tc.raw != "" && (refusal == nil || refusal.PullNumber != 6)) {
						t.Fatal("valid original predecessor rejected", err)
					}
				} else {
					var failure *client.Error
					if !errors.As(err, &failure) || failure.Code != "invalid_response" || strings.Contains(failure.Message, "PRIVATE_SENTINEL") {
						t.Fatal("invalid/private predecessor accepted", err)
					}
				}
				if reads != 1 {
					t.Fatal("read retried")
				}
			})
		}
	}
}
