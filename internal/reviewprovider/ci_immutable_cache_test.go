package reviewprovider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Clear only the baseline immutable cache between collections to model the prior
// adapter; every mutable endpoint and full collector remains the actual implementation.
func TestImmutableCommitCachePreservesFullRefreshFacts(t *testing.T) {
	for _, mode := range []string{"", "fallback", "wrong_app", "move_authority", "move_candidate", "move_results", "move_actions", "move_status_order", "move_empty_merge"} {
		t.Run(mode, func(t *testing.T) {
			baseline, cached := newCIObservationFixture(t, mode), newCIObservationFixture(t, mode)
			for round := 0; round < 2; round++ {
				baseline.g.mergeObjects = immutableMergeCache{}
				want, wantErr := baseline.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
				got, gotErr := cached.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatal(wantErr, gotErr)
				}
				stripCIObservationDesignTimes(&want)
				stripCIObservationDesignTimes(&got)
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("round %d facts differ\nwant=%+v\ngot=%+v", round, want, got)
				}
			}
			bp, bc := baseline.snapshot()
			cp, cc := cached.snapshot()
			for path, count := range bc {
				if path != candidateCommitPath && cc[path] != count {
					t.Fatal("mutable witness count changed", path, count, cc[path])
				}
			}
			if mode == "" && (len(bp) != 50 || len(cp) != 49 || bc[candidateCommitPath] != 2 || cc[candidateCommitPath] != 1) {
				t.Fatal(len(bp), len(cp), bc, cc)
			}
			t.Logf("mode=%q two-refresh requests %d -> %d; all non-commit witness counts identical", mode, len(bp), len(cp))
		})
	}
}
func stripCIObservationDesignTimes(v *CIObservation) {
	v.StartedAt = time.Time{}
	v.ObservedAt = time.Time{}
	v.Authority.ObservedAt = time.Time{}
	if v.Candidate != nil {
		v.Candidate.ObservedAt = time.Time{}
	}
	if v.Results != nil {
		v.Results.ObservedAt = time.Time{}
	}
	if v.TestMergeResults != nil {
		v.TestMergeResults.ObservedAt = time.Time{}
	}
	if v.Actions != nil {
		v.Actions.ObservedAt = time.Time{}
	}
	for i := range v.Attempts {
		v.Attempts[i].ObservedAt = time.Time{}
	}
}

func TestImmutableMergeCacheBoundsIsolationAndCopies(t *testing.T) {
	var cache immutableMergeCache
	value := githubCIMerge{SHA: candidateMerge}
	value.Parents = append(value.Parents, struct {
		SHA string `json:"sha,required"`
	}{candidateHead}, struct {
		SHA string `json:"sha,required"`
	}{candidateBase})
	cache.put("owner/repo", value)
	value.Parents[0].SHA = candidateMerge
	got, ok := cache.get("OWNER/REPO", candidateMerge)
	if !ok || got.Parents[0].SHA != candidateHead {
		t.Fatal("input alias or normalization", got)
	}
	got.Parents[0].SHA = candidateMerge
	got, _ = cache.get("owner/repo", candidateMerge)
	if got.Parents[0].SHA != candidateHead {
		t.Fatal("output alias")
	}
	if _, ok := cache.get("other/repo", candidateMerge); ok {
		t.Fatal("repository collision")
	}
	var other immutableMergeCache
	if _, ok := other.get("owner/repo", candidateMerge); ok {
		t.Fatal("adapter collision")
	}
	for i := 0; i < immutableMergeCacheCapacity; i++ {
		got.SHA = fmt.Sprintf("%040x", i+1)
		cache.put("owner/repo", got)
	}
	if len(cache.values) != immutableMergeCacheCapacity || len(cache.order) != immutableMergeCacheCapacity {
		t.Fatal("unbounded", len(cache.values), len(cache.order))
	}
	if _, ok := cache.get("owner/repo", candidateMerge); ok {
		t.Fatal("oldest not evicted")
	}
	invalid := got
	invalid.SHA = "invalid"
	cache.put("owner/repo", invalid)
	if len(cache.values) != immutableMergeCacheCapacity {
		t.Fatal("invalid admitted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cache.put("owner/repo", got)
				cache.get("owner/repo", got.SHA)
			}
		}()
	}
	wg.Wait()
}

func TestImmutableMergeCacheDoesNotBypassWarmMutableDeadline(t *testing.T) {
	f := newCIObservationFixture(t, "")
	if got, err := f.g.PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead); err != nil || got.State != "complete" {
		t.Fatal(got, err)
	}
	underlying := f.g.httpClient.Transport
	f.g.httpClient.Transport = ciRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == candidateBranchPath {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return underlying.RoundTrip(r)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	got, err := f.g.PullCICandidate(ctx, "owner/repo", 20, 7, candidateHead)
	if err == nil || got.State == "complete" || got.MergeSHA != "" {
		t.Fatal("cached object bypassed mutable timeout", got, err)
	}
	_, counts := f.snapshot()
	if counts[candidateCommitPath] != 1 {
		t.Fatal("immutable object unnecessarily reread", counts)
	}
}

func TestImmutableCacheCohortWarmPathKeepsMutableBrackets(t *testing.T) {
	f, targets := cohortFixture(t, "")
	first, err := f.g.pullCICandidateCohort(t.Context(), targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cold := f.snapshot()
	second, err := f.g.pullCICandidateCohort(t.Context(), targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, warm := f.snapshot()
	for i := range first {
		first[i].Candidate.ObservedAt = time.Time{}
		second[i].Candidate.ObservedAt = time.Time{}
		if first[i].Err != nil || second[i].Err != nil || first[i].Candidate.State != "complete" || !reflect.DeepEqual(first[i].Candidate, second[i].Candidate) {
			t.Fatal("warm cohort changed facts", first, second)
		}
	}
	if cold[candidateCommitPath] != 1 || warm[candidateCommitPath] != 1 {
		t.Fatal("immutable request not eliminated", cold, warm)
	}
	for path, count := range cold {
		if path != candidateCommitPath && warm[path] != 2*count {
			t.Fatal("mutable phase suppressed", path, cold, warm)
		}
	}
}
