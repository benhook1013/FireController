//go:build cohortintegration

package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type realCohortEngineReader struct {
	*sourceCITestReader
	github *reviewprovider.GitHub
}

func (r *realCohortEngineReader) NewCIGateCohort(ctx context.Context, targets []reviewprovider.CITarget, gate string) (*reviewprovider.CIGateCohort, error) {
	return r.github.NewCIGateCohort(ctx, targets, gate)
}

func TestLifecycleCohortRealProviderPublication(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	configureLifecycleTwoPulls(t, f)
	counts := map[string]int{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-cohort-integration" {
			t.Error("unexpected method/credential")
			http.Error(w, "invalid", 400)
			return
		}
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		var body any
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			body = map[string]any{"required_status_checks": nil}
		case "/repos/owner/repo/rules/branches/main":
			body = []any{}
		case "/repos/owner/repo/pulls/2", "/repos/owner/repo/pulls/4":
			var n int64
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/owner/repo/pulls/%d", &n)
			body = map[string]any{"title": "Synthetic pull", "number": n, "state": "open", "merged": false, "changed_files": 0, "mergeable": false, "merge_commit_sha": nil, "head": map[string]any{"sha": strings.Repeat("a", 40), "repo": map[string]any{"id": 801, "full_name": "owner/repo", "default_branch": "main"}}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": map[string]any{"id": 801, "full_name": "owner/repo", "default_branch": "main"}}}
		default:
			t.Errorf("unexpected provider request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	github, err := reviewprovider.NewCohortIntegrationFixture(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := &realCohortEngineReader{github: github, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }}}}
	lifecycleTick(t, f, s, r)
	lifecycleTick(t, f, s, r)
	clock.Add(6 * time.Minute)
	readers := map[string]TargetReader{"test-capability": r}
	first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	if err = f.e.observeLifecycleCohort(t.Context(), *first, readers, s); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["/repos/owner/repo/branches/main/protection"] != 3 || counts["/repos/owner/repo/rules/branches/main"] != 3 {
		t.Fatal("initial authority not shared with independent final reads", counts)
	}
	for _, n := range []int64{2, 4} {
		if counts[fmt.Sprintf("/repos/owner/repo/pulls/%d", n)] != 4 {
			t.Fatal("candidate bracket omitted", n, counts)
		}
		d := s.due[lifecycleKey{attemptProject, n}]
		if d.ciGateState != "not_required" || d.ciError != "" || !d.ci.After(clock.Now()) {
			t.Fatal("real provider evidence did not publish", n, d.ciGateState, d.ciError)
		}
	}
	if len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
		t.Fatal("reservation leak")
	}
}
