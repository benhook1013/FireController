package reviewprovider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func normalizeCohortObservation(o CIObservation) CIObservation {
	o.StartedAt, o.ObservedAt = time.Time{}, time.Time{}
	o.Authority.ObservedAt = time.Time{}
	if o.Candidate != nil {
		v := *o.Candidate
		v.ObservedAt = time.Time{}
		o.Candidate = &v
	}
	if o.Results != nil {
		v := *o.Results
		v.ObservedAt = time.Time{}
		o.Results = &v
	}
	if o.TestMergeResults != nil {
		v := *o.TestMergeResults
		v.ObservedAt = time.Time{}
		o.TestMergeResults = &v
	}
	if o.Actions != nil {
		v := *o.Actions
		v.ObservedAt = time.Time{}
		o.Actions = &v
	}
	if o.Attempts != nil {
		copied := make([]ActionsAttempt, len(o.Attempts))
		copy(copied, o.Attempts)
		o.Attempts = copied
	}
	for i := range o.Attempts {
		o.Attempts[i].ObservedAt = time.Time{}
	}
	return o
}

func TestCISourceCohortFullFactsAndCalls(t *testing.T) {
	for _, mode := range []string{"", "fallback", "empty", "pending", "conflicting", "wrong_app", "unlinked_suite", "newer_pending", "partial_merge", "context_capacity", "workflow_capacity", "stale_merge_parent", "cached_base_parent"} {
		t.Run(mode, func(t *testing.T) {
			baseline, targets := cohortFixture(t, mode)
			expected := make([]CIObservation, len(targets))
			errs := make([]error, len(targets))
			for i, target := range targets {
				expected[i], errs[i] = baseline.g.ObserveCIGate(t.Context(), target, "main")
			}
			before, bc := baseline.snapshot()
			f, targets := cohortFixture(t, mode)
			got, err := f.g.observeCIGateSourceCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			for i, row := range got {
				if !reflect.DeepEqual(normalizeCohortObservation(row.Observation), normalizeCohortObservation(expected[i])) {
					t.Fatalf("facts changed member%d\ngot%+v\nwant%+v", i, row.Observation, expected[i])
				}
				if (row.Err == nil) != (errs[i] == nil) {
					t.Fatal("error presence", i, row.Err, errs[i])
				}
			}
			after, ac := f.snapshot()
			if mode == "" {
				if len(before) != 49 || len(after) != 43 {
					t.Fatal("full green counts", len(before), len(after))
				}
				for path, n := range bc {
					want := n
					switch path {
					case candidateBranchPath:
						want -= 4
					case "/repos/owner/repo/branches/main/protection", "/repos/owner/repo/rules/branches/main":
						want--
					}
					if ac[path] != want {
						t.Fatal("unexpected endpoint change", path, ac[path], want)
					}
				}
				t.Log("2 full green targets: ordinary50,existing authority cohort48,source-phase cohort44; same facts and all final witnesses")
			}
		})
	}
}

func TestCISourceCohortPreservesMovementAndCancellation(t *testing.T) {
	for _, mode := range []string{"move_authority", "move_candidate", "move_results", "move_actions", "move_status_order", "move_empty_merge"} {
		t.Run(mode, func(t *testing.T) {
			f, targets := cohortFixture(t, mode)
			got, err := f.g.observeCIGateSourceCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Observation.State == "satisfied" || got[0].Observation.State == "not_required" {
				t.Fatal("changed first member credited", got[0])
			}
		})
	}
	f, targets := cohortFixture(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := f.g.observeCIGateSourceCohort(ctx, targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got {
		if row.Err == nil || row.Observation.State == "satisfied" {
			t.Fatal("canceled member credited", row)
		}
	}
	if p, _ := f.snapshot(); len(p) != 0 {
		t.Fatal("canceled cohort fetched provider", p)
	}
	if _, err = f.g.observeCIGateSourceCohort(t.Context(), append(targets, targets[0]), "main"); err == nil {
		t.Fatal("production-size limit relaxed")
	}
}

func TestCISourceCohortPartialFailureAndLateDeadline(t *testing.T) {
	for _, mode := range []string{"body_failure", "final_results_failure", "deadline_after_assessment"} {
		t.Run(mode, func(t *testing.T) {
			f := &ciObservationFixture{t: t, counts: map[string]int{}}
			var mu sync.Mutex
			suites, protection, callsAfterBlock := 0, 0, 0
			blocked := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if blocked {
					callsAfterBlock++
				}
				fail := false
				wait := false
				if strings.HasSuffix(r.URL.Path, "/check-suites") {
					suites++
					fail = mode == "body_failure" && suites == 1 || mode == "final_results_failure" && suites == 5
				}
				if strings.HasSuffix(r.URL.Path, "/protection") {
					protection++
					if mode == "deadline_after_assessment" && protection == 2 {
						blocked = true
						wait = true
					}
				}
				mu.Unlock()
				if wait {
					<-r.Context().Done()
					return
				}
				if fail {
					http.Error(w, "synthetic", 500)
					return
				}
				if r.URL.Path == "/repos/owner/repo/pulls/8" {
					clone := r.Clone(r.Context())
					u := *r.URL
					clone.URL = &u
					clone.URL.Path = candidatePullPath
					rec := httptest.NewRecorder()
					f.serve(rec, clone)
					for k, v := range rec.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(bytes.ReplaceAll(rec.Body.Bytes(), []byte(`"number":7`), []byte(`"number":8`)))
					return
				}
				f.serve(w, r)
			}))
			defer server.Close()
			f.g = testGitHub(server, "synthetic-ci-observation-token")
			a := ciObservationTarget()
			b := a
			b.PullNumber = 8
			ctx := t.Context()
			if mode == "deadline_after_assessment" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			}
			got, err := f.g.observeCIGateSourceCohort(ctx, []CITarget{a, b}, "main")
			if err != nil {
				t.Fatal(err)
			}
			stage := "merge_results"
			if mode == "final_results_failure" {
				stage = "results_recheck"
			}
			if mode == "deadline_after_assessment" {
				stage = "authority_recheck"
			}
			if got[0].Err == nil || CICollectionStage(got[0].Err) != stage || got[0].Observation.State == "satisfied" {
				t.Fatal("failed member credited/stage lost", got[0], CICollectionStage(got[0].Err))
			}
			if mode != "deadline_after_assessment" {
				if got[1].Err != nil || got[1].Observation.State != "satisfied" {
					t.Fatal("independent member contaminated", got[1])
				}
			} else {
				for _, row := range got {
					if row.Err == nil || row.Observation.State == "satisfied" || row.Observation.Assessment.State != "satisfied" {
						t.Fatal("late shared deadline lost provisional guard", row)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if !blocked || callsAfterBlock != 0 {
					t.Fatal("extra provider calls after deadline", blocked, callsAfterBlock)
				}
			}
		})
	}
}

func TestCISourceCohortOwnsEnrollment(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate", true: "full"}[full], func(t *testing.T) {
			f, targets := cohortFixture(t, "")
			original := f.g.httpClient.Transport
			if original == nil {
				original = http.DefaultTransport
			}
			first := true
			f.g.httpClient.Transport = ciRoundTripper(func(r *http.Request) (*http.Response, error) {
				if first {
					first = false
					targets[1].PullNumber = 99
				}
				return original.RoundTrip(r)
			})
			if full {
				got, err := f.g.observeCIGateSourceCohort(t.Context(), targets, "main")
				if err != nil {
					t.Fatal(err)
				}
				for i, row := range got {
					if row.Err != nil || row.Observation.State != "satisfied" || row.Observation.Target.PullNumber != int64(7+i) {
						t.Fatal("enrollment changed in flight", got)
					}
				}
			} else {
				got, err := f.g.pullCICandidateCohort(t.Context(), targets, nil)
				if err != nil {
					t.Fatal(err)
				}
				for i, row := range got {
					if row.Err != nil || row.Candidate.State != "complete" || row.Candidate.PullNumber != int64(7+i) {
						t.Fatal("candidate enrollment changed in flight", got)
					}
				}
			}
		})
	}
}
