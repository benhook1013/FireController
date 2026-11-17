package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewSourceUnconfiguredCarriedObligations(t *testing.T) {
	for _, mode := range []string{"policy", "readiness"} {
		t.Run(mode, func(t *testing.T) {
			var response atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/reviews/source/2/"+mode || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("source assessment changed bound read authority")
				}
				write(w, http.StatusOK, response.Load())
			}))
			defer srv.Close()
			c := newClient(t, srv)
			read := func() (any, error) {
				if mode == "policy" {
					return c.ReviewSourcePolicyAssessment(t.Context(), 2)
				}
				return c.ReviewSourceReadiness(t.Context(), 2)
			}
			base := func(pending int64) map[string]any {
				var value any
				if mode == "policy" {
					v := sampleSourceAssessment()
					v.State, v.PolicyRevision = "unconfigured", 0
					v.Channels = []client.ReviewPolicyAssessmentChannel{}
					v.SourceObligationsPending = pending
					if pending > 0 {
						v.Reason = "source_obligations_pending"
					}
					value = v
				} else {
					v := sampleReadiness()
					v.State, v.PolicyRevision = "unconfigured", 0
					v.Channels = []client.ReviewPolicyAssessmentChannel{}
					v.SourceObligationsPending = pending
					v.Reasons = []string{"policy_unconfigured"}
					if pending > 0 {
						v.Reasons = append(v.Reasons, "source_obligations_pending")
					}
					value = v
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]any
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			for _, pending := range []int64{0, 1, (1 << 53) - 1} {
				want := base(pending)
				response.Store(want)
				got, err := read()
				if err != nil {
					t.Error("accurate unconfigured source obligations rejected", pending, err)
					continue
				}
				raw, _ := json.Marshal(got)
				var actual map[string]any
				if json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, want) {
					t.Error("source assessment changed retained shape or independent obligations", got, want)
				}
			}
			for name, edit := range map[string]func(map[string]any){
				"missing count":     func(v map[string]any) { delete(v, "source_obligations_pending") },
				"zero with reason":  func(v map[string]any) { v["source_obligations_pending"] = 0 },
				"negative count":    func(v map[string]any) { v["source_obligations_pending"] = -1 },
				"unsafe count":      func(v map[string]any) { v["source_obligations_pending"] = int64(1 << 53) },
				"fractional count":  func(v map[string]any) { v["source_obligations_pending"] = 1.5 },
				"null count":        func(v map[string]any) { v["source_obligations_pending"] = nil },
				"positive revision": func(v map[string]any) { v["policy_revision"] = 1 },
				"null channels":     func(v map[string]any) { v["channels"] = nil },
				"nonempty channels": func(v map[string]any) {
					v["channels"] = []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}
				},
				"missing obligation reason": func(v map[string]any) {
					if mode == "policy" {
						delete(v, "reason")
					} else {
						v["reasons"] = []string{"policy_unconfigured"}
					}
				},
			} {
				t.Run(name, func(t *testing.T) {
					value := base(1)
					edit(value)
					response.Store(value)
					_, err := read()
					checkError(t, err, "invalid_response")
				})
			}
		})
	}
}
