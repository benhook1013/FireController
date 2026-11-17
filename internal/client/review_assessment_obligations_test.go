package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewPolicyAssessmentUnconfiguredCarriedObligations(t *testing.T) {
	var response atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/reviews/evidence/"+controlEvidenceID+"/policy" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("unconfigured obligation assessment changed bound read authority")
		}
		write(w, http.StatusOK, response.Load())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	base := func() map[string]any {
		value := samplePolicyAssessment()
		value.State, value.PolicyRevision = "unconfigured", 0
		value.Channels = []client.ReviewPolicyAssessmentChannel{}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, test := range []struct {
		name    string
		pending int64
		edit    func(map[string]any)
	}{
		{"omitted zero", 0, func(v map[string]any) {}},
		{"explicit zero", 0, func(v map[string]any) { v["source_obligations_pending"] = 0; v["reason"] = "" }},
		{"one carried obligation", 1, func(v map[string]any) {
			v["source_obligations_pending"] = 1
			v["reason"] = "source_obligations_pending"
		}},
		{"maximum exact count", (1 << 53) - 1, func(v map[string]any) {
			v["source_obligations_pending"] = int64((1 << 53) - 1)
			v["reason"] = "source_obligations_pending"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base()
			test.edit(value)
			response.Store(value)
			got, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
			if err != nil || got.State != "unconfigured" || got.PolicyRevision != 0 || got.Channels == nil || len(got.Channels) != 0 || got.SourceObligationsPending != test.pending {
				t.Fatal("unconfigured assessment lost accurate carried obligations", got, err)
			}
		})
	}
	for name, edit := range map[string]func(map[string]any){
		"missing count":     func(v map[string]any) { delete(v, "source_obligations_pending") },
		"zero with reason":  func(v map[string]any) { v["source_obligations_pending"] = 0 },
		"negative count":    func(v map[string]any) { v["source_obligations_pending"] = -1 },
		"unsafe count":      func(v map[string]any) { v["source_obligations_pending"] = int64(1 << 53) },
		"fractional count":  func(v map[string]any) { v["source_obligations_pending"] = 1.5 },
		"string count":      func(v map[string]any) { v["source_obligations_pending"] = "1" },
		"null count":        func(v map[string]any) { v["source_obligations_pending"] = nil },
		"missing reason":    func(v map[string]any) { delete(v, "reason") },
		"empty reason":      func(v map[string]any) { v["reason"] = "" },
		"wrong reason":      func(v map[string]any) { v["reason"] = "routed_findings_pending" },
		"null reason":       func(v map[string]any) { v["reason"] = nil },
		"positive revision": func(v map[string]any) { v["policy_revision"] = 1 },
		"negative revision": func(v map[string]any) { v["policy_revision"] = -1 },
		"null channels":     func(v map[string]any) { v["channels"] = nil },
		"nonempty channels": func(v map[string]any) {
			v["channels"] = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
		},
		"false sufficient":    func(v map[string]any) { v["state"] = "sufficient" },
		"held without policy": func(v map[string]any) { v["state"] = "held" },
		"incoming pending":    func(v map[string]any) { v["incoming_routes_pending"] = 1; v["reason"] = "routed_findings_pending" },
		"incoming capacity":   func(v map[string]any) { v["incoming_routes_capacity"] = true; v["reason"] = "routed_findings_capacity" },
	} {
		t.Run(name, func(t *testing.T) {
			value := base()
			value["source_obligations_pending"], value["reason"] = 1, "source_obligations_pending"
			edit(value)
			response.Store(value)
			_, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
			checkError(t, err, "invalid_response")
		})
	}
}
