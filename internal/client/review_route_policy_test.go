package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewRoutePolicyOptionalFieldsAndIndependentIncomingHolds(t *testing.T) {
	for _, source := range []bool{false, true} {
		name := "captured"
		if source {
			name = "observed"
		}
		t.Run(name, func(t *testing.T) {
			var response atomic.Value
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("assessment performed mutation/provider selector")
				}
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				want := "/api/v1/projects/project-one/reviews/evidence/" + controlEvidenceID + "/policy"
				if source {
					want = "/api/v1/projects/project-one/reviews/source/2/policy"
				}
				if r.URL.Path != want || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("lost bound assessment identity")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response.Load().(string)))
			}))
			defer srv.Close()
			c := newClient(t, srv)
			base := func() map[string]any {
				var raw []byte
				if source {
					v := sampleSourceAssessment()
					v.State = "sufficient"
					v.Channels = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
					raw, _ = json.Marshal(v)
				} else {
					v := samplePolicyAssessment()
					v.State = "sufficient"
					v.Channels = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
					raw, _ = json.Marshal(v)
				}
				var v map[string]any
				_ = json.Unmarshal(raw, &v)
				return v
			}
			read := func() error {
				if source {
					_, err := c.ReviewSourcePolicyAssessment(t.Context(), 2)
					return err
				}
				_, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
				return err
			}
			for _, test := range []struct {
				name string
				edit func(map[string]any)
			}{
				{"omitted optional zero", func(v map[string]any) {}},
				{"explicit zero", func(v map[string]any) { v["incoming_routes_pending"] = 0; v["incoming_routes_capacity"] = false }},
				{"incoming one", func(v map[string]any) {
					v["state"] = "held"
					v["reason"] = "routed_findings_pending"
					v["incoming_routes_pending"] = 1
				}},
				{"incoming exact 100", func(v map[string]any) {
					v["state"] = "held"
					v["reason"] = "routed_findings_pending"
					v["incoming_routes_pending"] = 100
				}},
				{"capacity is not invented count", func(v map[string]any) {
					v["state"] = "held"
					v["reason"] = "routed_findings_capacity"
					v["incoming_routes_capacity"] = true
				}},
				{"channel still pending", func(v map[string]any) {
					v["state"] = "held"
					v["channels"].([]any)[0].(map[string]any)["pending"] = true
				}},
				{"unconfigured", func(v map[string]any) { v["state"] = "unconfigured"; v["policy_revision"] = 0; v["channels"] = []any{} }},
			} {
				t.Run(test.name, func(t *testing.T) {
					v := base()
					test.edit(v)
					raw, _ := json.Marshal(v)
					response.Store(string(raw))
					before := calls.Load()
					if err := read(); err != nil {
						t.Fatal("valid bounded route state rejected", err)
					}
					if calls.Load()-before != 1 {
						t.Fatal("read retried/polled", calls.Load()-before)
					}
				})
			}
			if source {
				v := base()
				v["state"] = "held"
				v["reason"] = "source_configuration_changed"
				v["channels"] = []any{}
				v["configuration_revision"] = 0
				raw, _ := json.Marshal(v)
				response.Store(string(raw))
				if err := read(); err != nil {
					t.Fatal("distinct stale-source hold rejected", err)
				}
			}
		})
	}
}

func TestReviewRoutePolicyRejectsMalformedOrContradictoryIncomingFacts(t *testing.T) {
	for _, source := range []bool{false, true} {
		name := "captured"
		if source {
			name = "observed"
		}
		t.Run(name, func(t *testing.T) {
			var response atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				if r.Method != "GET" {
					t.Error("read used a write method")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response.Load().(string)))
			}))
			defer srv.Close()
			c := newClient(t, srv)
			base := func() map[string]any {
				var raw []byte
				if source {
					v := sampleSourceAssessment()
					v.State = "held"
					v.Channels = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
					raw, _ = json.Marshal(v)
				} else {
					v := samplePolicyAssessment()
					v.State = "held"
					v.Channels = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
					raw, _ = json.Marshal(v)
				}
				var v map[string]any
				_ = json.Unmarshal(raw, &v)
				v["incoming_routes_pending"] = 1
				v["reason"] = "routed_findings_pending"
				return v
			}
			read := func() error {
				if source {
					_, err := c.ReviewSourcePolicyAssessment(t.Context(), 2)
					return err
				}
				_, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
				return err
			}
			cases := []struct {
				name string
				edit func(map[string]any)
			}{
				{"false sufficient", func(v map[string]any) { v["state"] = "sufficient" }},
				{"negative count", func(v map[string]any) { v["incoming_routes_pending"] = -1 }},
				{"over capacity exact count", func(v map[string]any) { v["incoming_routes_pending"] = 101 }},
				{"null count", func(v map[string]any) { v["incoming_routes_pending"] = nil }},
				{"fraction count", func(v map[string]any) { v["incoming_routes_pending"] = 1.5 }},
				{"string count", func(v map[string]any) { v["incoming_routes_pending"] = "1" }},
				{"null capacity", func(v map[string]any) { v["incoming_routes_capacity"] = nil }},
				{"numeric capacity", func(v map[string]any) { v["incoming_routes_capacity"] = 1 }},
				{"missing pending reason", func(v map[string]any) { delete(v, "reason") }},
				{"unknown reason", func(v map[string]any) { v["reason"] = "ready" }},
				{"pending reason without pending", func(v map[string]any) { v["incoming_routes_pending"] = 0 }},
				{"capacity with exact pending count", func(v map[string]any) { v["incoming_routes_capacity"] = true; v["reason"] = "routed_findings_capacity" }},
				{"capacity pending reason", func(v map[string]any) { v["incoming_routes_pending"] = 0; v["incoming_routes_capacity"] = true }},
				{"capacity reason without flag", func(v map[string]any) { v["incoming_routes_pending"] = 0; v["reason"] = "routed_findings_capacity" }},
				{"sufficient capacity", func(v map[string]any) {
					v["incoming_routes_pending"] = 0
					v["incoming_routes_capacity"] = true
					v["reason"] = "routed_findings_capacity"
					v["state"] = "sufficient"
				}},
				{"unconfigured with incoming", func(v map[string]any) { v["state"] = "unconfigured"; v["policy_revision"] = 0; v["channels"] = []any{} }},
			}
			cases = append(cases,
				struct {
					name string
					edit func(map[string]any)
				}{"unconfigured silent pending", func(v map[string]any) {
					v["state"] = "unconfigured"
					v["policy_revision"] = 0
					v["channels"] = []any{}
					delete(v, "reason")
				}},
				struct {
					name string
					edit func(map[string]any)
				}{"unconfigured silent capacity", func(v map[string]any) {
					v["state"] = "unconfigured"
					v["policy_revision"] = 0
					v["channels"] = []any{}
					v["incoming_routes_pending"] = 0
					v["incoming_routes_capacity"] = true
					delete(v, "reason")
				}},
			)
			if source {
				cases = append(cases, struct {
					name string
					edit func(map[string]any)
				}{"stale source negative count", func(v map[string]any) {
					v["reason"] = "source_configuration_changed"
					v["channels"] = []any{}
					v["incoming_routes_pending"] = -1
				}}, struct {
					name string
					edit func(map[string]any)
				}{"stale source contradictory capacity", func(v map[string]any) {
					v["reason"] = "source_configuration_changed"
					v["channels"] = []any{}
					v["incoming_routes_pending"] = 0
					v["incoming_routes_capacity"] = true
				}})
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					v := base()
					test.edit(v)
					raw, _ := json.Marshal(v)
					response.Store(string(raw))
					checkError(t, read(), "invalid_response")
				})
			}
			// Optional additions still obey the strict response key rules.
			raw, _ := json.Marshal(base())
			for _, bad := range []string{strings.Replace(string(raw), `"incoming_routes_pending":`, `"Incoming_routes_pending":`, 1), strings.Replace(string(raw), `"incoming_routes_pending":`, `"incoming_routes_pending":1,"incoming_routes_pending":`, 1)} {
				response.Store(bad)
				checkError(t, read(), "invalid_response")
			}
		})
	}
}
