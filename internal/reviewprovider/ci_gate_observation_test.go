package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const gateActualBase = "stack/parent"

// Reuse the established candidate/results fixture; only the distinct authority
// and actual base paths differ. Record original requests before routing the
// fixture's existing main-branch source responses to the selected parent.
func newCIGateFixture(t *testing.T, mode string) (*GitHub, CITarget, func() []string) {
	t.Helper()
	underlying := mode
	switch mode {
	case "preview_failure", "empty", "unknown_authority", "move_base", "move_gate":
		underlying = "merge"
	}
	f := &ciObservationFixture{t: t, mode: underlying, counts: map[string]int{}}
	var mu sync.Mutex
	var paths []string
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		counts[r.URL.Path]++
		call := counts[r.URL.Path]
		mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-ci-observation-token" {
			t.Error("gate observation changed method or credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			if mode == "unknown_authority" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if mode == "empty" {
				_ = json.NewEncoder(w).Encode(map[string]any{"required_status_checks": nil})
				return
			}
			context := "Main validate"
			if mode == "move_gate" && call > 1 {
				context = "changed gate"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"required_status_checks": map[string]any{"strict": true, "checks": []any{map[string]any{"context": context, "app_id": 10}}, "contexts": []string{}}})
			return
		case "/repos/owner/repo/rules/branches/main":
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		copy := r.Clone(r.Context())
		url := *r.URL
		copy.URL = &url
		if r.URL.Path == "/repos/owner/repo/branches/"+gateActualBase {
			copy.URL.Path = candidateBranchPath
			copy.URL.RawPath = ""
		}
		recorded := httptest.NewRecorder()
		f.serve(recorded, copy)
		if recorded.Code != http.StatusOK {
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		var body any
		if json.Unmarshal(recorded.Body.Bytes(), &body) != nil {
			t.Error("invalid base fixture JSON")
			return
		}
		if r.URL.Path == candidatePullPath {
			pull := body.(map[string]any)
			base := pull["base"].(map[string]any)
			base["ref"] = gateActualBase
			if mode == "move_base" && call > 2 {
				base["sha"] = strings.Repeat("d", 40)
			}
		}
		if r.URL.Path == "/repos/owner/repo/branches/"+gateActualBase {
			body.(map[string]any)["name"] = gateActualBase
		}
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			envelope := body.(map[string]any)
			checks := envelope["check_runs"].([]any)
			for _, value := range checks {
				check := value.(map[string]any)
				if check["name"] == "build" {
					check["name"] = "Main validate"
				}
			}
			if mode == "preview_failure" {
				preview := ciCheckFixture(1080)
				preview["name"] = "Preview deploy"
				preview["head_sha"] = candidateMerge
				preview["check_suite"] = map[string]any{"id": 41}
				preview["conclusion"] = "failure"
				envelope["check_runs"] = append(checks, preview)
				envelope["total_count"] = len(checks) + 1
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	target := ciObservationTarget()
	target.BaseRef = gateActualBase
	return testGitHub(server, "synthetic-ci-observation-token"), target, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

func TestObserveCIGateSelectsOnlyFinalBranchRequirementsForActualStackComparison(t *testing.T) {
	g, target, reads := newCIGateFixture(t, "preview_failure")
	got, err := g.ObserveCIGate(t.Context(), target, "main")
	if err != nil || got.State != "satisfied" || got.Target != target || got.Authority.Branch != "main" || len(got.Authority.Checks) != 1 || got.Authority.Checks[0].Context != "Main validate" || len(got.Assessment.Checks) != 1 || got.Assessment.Checks[0].Context != "Main validate" || got.Candidate == nil || got.Candidate.BaseRef != gateActualBase || got.Candidate.BaseSHA != target.BaseSHA || got.SelectedCommitSHA != candidateMerge || got.Selection != "test_merge" || len(got.Attempts) != 1 || got.Attempts[0].RunID != 50 {
		t.Fatal("gate requirements replaced actual comparison or pulled optional workflow", got, err)
	}
	optional := false
	for _, check := range got.Results.Checks {
		optional = optional || check.Name == "Preview deploy" && check.Conclusion == "failure"
	}
	if !optional {
		t.Fatal("fixture did not include failed optional preview")
	}
	branches, protection, rules := 0, 0, 0
	for _, path := range reads() {
		path = strings.TrimSuffix(path, "?")
		switch path {
		case "/repos/owner/repo/branches/stack%2Fparent":
			branches++
		case "/repos/owner/repo/branches/main/protection":
			protection++
		case "/repos/owner/repo/rules/branches/main?page=1&per_page=100":
			rules++
		}
		if strings.Contains(path, "/branches/stack%2Fparent/protection") || strings.Contains(path, "/rules/branches/stack") || path == candidateBranchPath || strings.Contains(path, "/runs/60/") || strings.Contains(path, "/commits/"+candidateHead+"/") {
			t.Fatal("wrong authority/source/workflow fanout", path)
		}
	}
	if branches != 4 || protection != 2 || rules != 2 {
		t.Fatal("missing independently bound branch or authority rechecks", branches, protection, rules, reads())
	}
}

func TestObserveCIGateEmptyUnknownAndMovingFactsRemainTruthful(t *testing.T) {
	for _, mode := range []string{"empty", "unknown_authority", "move_gate", "move_candidate", "move_base", "move_actions", "stale_merge_parent", "newer_pending"} {
		t.Run(mode, func(t *testing.T) {
			g, target, reads := newCIGateFixture(t, mode)
			got, err := g.ObserveCIGate(t.Context(), target, "main")
			if mode == "empty" {
				if err != nil || got.State != "not_required" || got.Assessment.State != "not_required" || got.Authority.Branch != "main" || got.Results != nil || got.Actions != nil || got.Attempts != nil {
					t.Fatal("empty gate invented green/results", got, err)
				}
				for _, path := range reads() {
					if strings.Contains(path, "/actions/") || strings.Contains(path, "/commits/") && !strings.Contains(path, "/git/commits/") {
						t.Fatal("empty gate fetched optional results", path)
					}
				}
				return
			}
			if got.State == "satisfied" || got.State == "not_required" || got.Target != target {
				t.Fatal("unknown or moved gate/source/attempt became green", got, err)
			}
			if mode == "unknown_authority" {
				if err == nil || got.Candidate != nil || len(reads()) != 1 {
					t.Fatal("unknown authority continued collection", got, err, reads())
				}
			} else if err != nil {
				t.Fatal("movement became transport failure", err)
			}
		})
	}
}

func TestObserveCIGateRejectsInvalidGateBeforeIOAndRetainsOriginalContract(t *testing.T) {
	f := newCIObservationFixture(t, "merge")
	for _, gate := range []string{"", "../main", "main\n", "refs//heads/main"} {
		if _, err := f.g.ObserveCIGate(t.Context(), ciObservationTarget(), gate); err == nil {
			t.Fatal("invalid gate accepted", gate)
		}
	}
	paths, _ := f.snapshot()
	if len(paths) != 0 {
		t.Fatal("invalid gate performed provider I/O", paths)
	}
	first := newCIObservationFixture(t, "merge")
	second := newCIObservationFixture(t, "merge")
	a, errA := first.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	b, errB := second.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	pa, ca := first.snapshot()
	pb, cb := second.snapshot()
	for i := range a.Attempts {
		a.Attempts[i].ObservedAt = time.Time{}
	}
	for i := range b.Attempts {
		b.Attempts[i].ObservedAt = time.Time{}
	}
	if errA != nil || errB != nil || a.State != b.State || a.Reason != b.Reason || a.Target != b.Target || a.Selection != b.Selection || a.SelectedCommitSHA != b.SelectedCommitSHA || !sameCIAuthority(a.Authority, b.Authority) || !sameCICandidate(*a.Candidate, *b.Candidate) || !sameCIResults(*a.Results, *b.Results) || !sameCIActions(*a.Actions, *b.Actions) || !reflect.DeepEqual(a.Attempts, b.Attempts) || !reflect.DeepEqual(a.Assessment, b.Assessment) || !reflect.DeepEqual(pa, pb) || !reflect.DeepEqual(ca, cb) {
		t.Fatal("existing base-ref observation contract changed", errA, errB)
	}
}
