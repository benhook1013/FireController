package reviewprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// GraphQL fixtures expose the same suite facts as the existing REST fixture.
// The counter measures real client/server requests, not the local fixture's
// internal fact lookup; no REST request is issued to implement GraphQL.
func fullCountCohortFixture(t *testing.T, mode string) (*GitHub, []CITarget, *atomic.Int64) {
	return fullCountCohortFixtureHook(t, mode, nil)
}

func fullCountCohortFixtureHook(t *testing.T, mode string, hook func(http.ResponseWriter, *http.Request) bool) (*GitHub, []CITarget, *atomic.Int64) {
	t.Helper()
	f := &ciObservationFixture{t: t, mode: mode, counts: map[string]int{}}
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if hook != nil && hook(w, r) {
			return
		}
		if r.URL.Path == "/graphql" {
			var req struct{ Variables map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			nodes := [2]any{}
			for i, key := range []string{"h0", "h1"} {
				head := req.Variables[key]
				get := r.Clone(r.Context())
				u := *r.URL
				get.URL = &u
				get.Method = "GET"
				get.URL.Path = "/repos/owner/repo/commits/" + head + "/check-suites"
				get.URL.RawQuery = "page=1&per_page=1"
				rec := httptest.NewRecorder()
				f.serve(rec, get)
				var facts struct {
					Total int64 `json:"total_count"`
				}
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &facts) != nil {
					t.Error("bad fixture count")
					w.WriteHeader(503)
					return
				}
				nodes[i] = suiteBatchNode(head, int(facts.Total))
			}
			json.NewEncoder(w).Encode(suiteBatchEnvelope(nodes[0], nodes[1]))
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls/8" {
			r = r.Clone(r.Context())
			u := *r.URL
			r.URL = &u
			r.URL.Path = candidatePullPath
			rec := httptest.NewRecorder()
			f.serve(rec, r)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(bytes.ReplaceAll(rec.Body.Bytes(), []byte(`"number":7`), []byte(`"number":8`)))
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(server.Close)
	g := testGitHub(server, "synthetic-ci-observation-token")
	a := ciObservationTarget()
	b := a
	b.PullNumber = 8
	return g, []CITarget{a, b}, calls
}

func TestCIFullCountCohortFactsAndRequests(t *testing.T) {
	for _, mode := range []string{"", "fallback", "empty", "pending", "conflicting", "wrong_app", "unlinked_suite", "newer_pending", "partial_merge", "context_capacity", "workflow_capacity", "stale_merge_parent", "cached_base_parent"} {
		t.Run(mode, func(t *testing.T) {
			old, targets := cohortFixture(t, mode)
			want, err := old.g.observeCIGateSourceCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			g, targets, calls := fullCountCohortFixture(t, mode)
			got, err := g.observeCIGateBatchedCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				if !reflect.DeepEqual(normalizeCohortObservation(got[i].Observation), normalizeCohortObservation(want[i].Observation)) || (got[i].Err == nil) != (want[i].Err == nil) {
					t.Fatalf("member%d facts differ\ngot%+v\nwant%+v", i, got[i], want[i])
				}
			}
			if mode == "" && calls.Load() != 40 {
				t.Fatalf("full green physical requests=%d want40", calls.Load())
			}
			t.Logf("mode=%q physical requests=%d", mode, calls.Load())
		})
	}
}

func TestCIFullCountCohortMovement(t *testing.T) {
	for _, mode := range []string{"move_authority", "move_candidate", "move_results", "move_actions", "move_status_order", "move_empty_merge"} {
		t.Run(mode, func(t *testing.T) {
			g, targets, _ := fullCountCohortFixture(t, mode)
			out, err := g.observeCIGateBatchedCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			if out[0].Observation.State == "satisfied" || out[0].Observation.State == "not_required" {
				t.Fatal("moving first member credited", out[0])
			}
		})
	}
}

func TestCIFullCountCohortPartialFailuresAndFinalCancel(t *testing.T) {
	for _, mode := range []string{"first_body", "first_final", "final_cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var checks, graphql atomic.Int64
			g, targets, _ := fullCountCohortFixtureHook(t, "", func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/repos/owner/repo/commits/"+candidateMerge+"/check-runs" {
					n := checks.Add(1)
					if mode == "first_body" && n == 1 {
						w.WriteHeader(503)
						return true
					}
				}
				if r.URL.Path == "/graphql" && graphql.Add(1) == 3 {
					// Each paired inventory has two GraphQL count brackets; the third
					// physical GraphQL request begins the final selected-inventory phase.
					if mode == "first_final" {
						json.NewEncoder(w).Encode(suiteBatchEnvelope(nil, suiteBatchNode(candidateMerge, 2)))
						return true
					}
					if mode == "final_cancel" {
						cancel()
					}
				}
				return false
			})
			got, err := g.observeCIGateBatchedCohort(ctx, targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Err == nil || got[0].Observation.State == "satisfied" {
				t.Fatal("first failure credited", got[0])
			}
			if mode == "final_cancel" {
				if got[1].Err == nil || got[1].Observation.State == "satisfied" || checks.Load() < 2 {
					t.Fatal("late cancel credited", got, checks.Load())
				}
			} else if got[1].Err != nil || got[1].Observation.State != "satisfied" {
				t.Fatal("healthy member lost", got[1])
			}
		})
	}
}
