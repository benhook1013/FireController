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
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type lateBatchEngineReader struct {
	*sourceCITestReader
	github *reviewprovider.GitHub
}

func (r *lateBatchEngineReader) NewCIGateCohort(ctx context.Context, targets []reviewprovider.CITarget, gate string) (*reviewprovider.CIGateCohort, error) {
	return r.github.NewCIGateCohort(ctx, targets, gate)
}

func (r *lateBatchEngineReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	return r.github.ObserveCIGate(ctx, target, gate)
}

func TestLifecycleBatchedCohortDoesNotCreditLateWatch(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	configureLifecycleTwoPulls(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var blockOnce sync.Once
	var blockEnabled atomic.Bool
	defer func() {
		blockOnce.Do(func() { close(entered) })
		select {
		case <-release:
		default:
			close(release)
		}
	}()
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
			if blockEnabled.Load() {
				blockOnce.Do(func() { close(entered); <-release })
			}
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
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		server.Close()
	}()
	github, err := reviewprovider.NewCohortIntegrationFixture(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := &lateBatchEngineReader{github: github, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }}}}
	lifecycleTick(t, f, s, r)
	lifecycleTick(t, f, s, r)
	clock.Add(6 * time.Minute)
	mu.Lock()
	counts = map[string]int{}
	mu.Unlock()
	blockEnabled.Store(true)
	s.options.BatchCIWitnesses = true
	s.readers = map[string]TargetReader{"test-capability": r}
	attachLifecycleHints(t, f, s)
	readers := map[string]TargetReader{"test-capability": r}
	first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycleCohort(t.Context(), *first, readers, s) }()
	<-entered
	watch, cancelWatch := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: first.number, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}})
	defer cancelWatch()
	ciWatchRegistered(t, s, 1)
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-watch:
		t.Fatal("late subscriber received pre-registration collection", reply)
	default:
	}
	s.mu.Lock()
	for _, entry := range s.watches {
		if entry.version != 0 {
			s.mu.Unlock()
			t.Fatal("pre-registration evidence credited to late watch", entry.version)
		}
	}
	s.mu.Unlock()

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

func TestLifecycleBatchedCohortStopsPartnerAfterProviderLimit(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, partner := range []bool{false, true} {
			t.Run(fmt.Sprintf("status%d_partner%t", status, partner), func(t *testing.T) {
				testLifecycleBatchedCohortStopsAccount(t, status, partner)
			})
		}
	}
}

func testLifecycleBatchedCohortStopsAccount(t *testing.T, status int, partner bool) {
	f, s, clock := lifecycleFixture(t)
	configureLifecycleTwoPulls(t, f)
	counts := map[string]int{}
	var failureTarget int64
	failed := false
	callsAfterFailure := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-cohort-integration" {
			t.Error("unexpected method/credential")
			http.Error(w, "invalid", 400)
			return
		}
		mu.Lock()
		counts[r.URL.Path]++
		if failed {
			callsAfterFailure++
		}
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
			if n == failureTarget {
				mu.Lock()
				failed = true
				mu.Unlock()
				w.Header().Set("Retry-After", "120")
				http.Error(w, "synthetic provider failure", status)
				return
			}
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
	r := &lateBatchEngineReader{github: github, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }}}}
	lifecycleTick(t, f, s, r)
	lifecycleTick(t, f, s, r)
	clock.Add(6 * time.Minute)
	s.options.BatchCIWitnesses = true
	readers := map[string]TargetReader{"test-capability": r}
	first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	failureTarget = first.number
	if partner {
		if failureTarget == 2 {
			failureTarget = 4
		} else {
			failureTarget = 2
		}
	}
	before := map[lifecycleKey]lifecycleDue{}
	for _, n := range []int64{2, 4} {
		key := lifecycleKey{attemptProject, n}
		before[key] = s.due[key]
	}
	if err = f.e.observeLifecycleCohort(t.Context(), *first, readers, s); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !failed {
		t.Fatal("failure injection not reached")
	}
	if callsAfterFailure != 0 {
		t.Fatal("provider calls continued after directed account limit", callsAfterFailure, counts)
	}
	if !s.accounts[first.account].until.After(clock.Now()) {
		t.Fatal("account cooldown not retained")
	}
	for key, prior := range before {
		if s.due[key] != prior {
			t.Fatal("failed batch altered per-target evidence", key, prior, s.due[key])
		}
	}

	if len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
		t.Fatal("reservation leak")
	}
}
