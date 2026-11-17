package reviewexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestLifecycleUnpublishedEnrichmentDoesNotRepeatProviderReads(t *testing.T) {
	for _, mode := range []string{"moved_head", "invalid_metadata", "inconclusive", "throttled"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			pull := lifecyclePull(2)
			var failure error
			calls := 0
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { calls++; return pull, failure }}}
			readers := map[string]TargetReader{"test-capability": r}
			observe := func(lane lifecycleLane) bool {
				t.Helper()
				target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
				if err != nil {
					t.Fatal(err)
				}
				if target == nil {
					return false
				}
				if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
					t.Fatal(err)
				}
				return true
			}
			if !observe(lifecycleMetadata) {
				t.Fatal("missing initial metadata claim")
			}
			switch mode {
			case "moved_head":
				pull.HeadSHA = strings.Repeat("c", 40)
			case "invalid_metadata":
				pull.HeadSHA = strings.Repeat("c", 40)
				pull.Title = "unsupported title\x01"
				clock.Add(time.Minute)
				if !observe(lifecycleMetadata) {
					t.Fatal("missing changed metadata claim")
				}
				if lifecycleRead(t, f).HeadSHA == pull.HeadSHA {
					t.Fatal("invalid metadata was retained")
				}
			case "inconclusive":
				failure = &reviewprovider.Error{Code: "provider_invalid_response"}
			case "throttled":
				failure = &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120}
			}
			before := calls
			if !observe(lifecycleEnrichment) {
				t.Fatal("missing first enrichment claim")
			}
			repeated := 0
			for range 8 {
				if observe(lifecycleEnrichment) {
					repeated++
				}
			}
			if repeated != 0 || calls-before != 1 {
				t.Fatalf("unpublished enrichment immediately reclaimed %d times, provider Pull calls=%d; want one", repeated, calls-before)
			}
			if r.ciCalls.Load() != 0 {
				t.Fatal("unpublished comparison gained CI collection")
			}
			clock.Add(time.Minute)
			if mode == "throttled" {
				if observe(lifecycleEnrichment) {
					t.Fatal("shared cooldown was bypassed")
				}
				clock.Add(time.Minute)
			}
			if !observe(lifecycleEnrichment) {
				t.Fatal("ordinary retry cadence stopped")
			}
		})
	}
}

func TestLifecycleEnrichmentFinishPreservesConcurrentComparisonWake(t *testing.T) {
	for _, field := range []string{"head", "base_sha", "base_ref"} {
		t.Run(field, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			pull := lifecyclePull(2)
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }}}
			readers := map[string]TargetReader{"test-capability": r}
			claim := func(lane lifecycleLane) *lifecycleTarget {
				t.Helper()
				target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
				if err != nil || target == nil {
					t.Fatal("expected claim", target, err)
				}
				return target
			}
			metadata := claim(lifecycleMetadata)
			if err := f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
				t.Fatal(err)
			}
			enrichment := claim(lifecycleEnrichment)
			// Metadata completes for a newer comparison while enrichment owns the old
			// claim. Completion must consume only that old wake, not the new one.
			clock.Add(time.Minute)
			switch field {
			case "head":
				pull.HeadSHA = strings.Repeat("c", 40)
			case "base_sha":
				pull.BaseSHA = strings.Repeat("d", 40)
			case "base_ref":
				pull.BaseRef = "new-base"
			}
			metadata = claim(lifecycleMetadata)
			if err := f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
				t.Fatal(err)
			}
			if err := f.e.observeLifecycle(t.Context(), *enrichment, s); err != nil {
				t.Fatal(err)
			}
			if !clock.Now().Before(s.due[lifecycleKey{attemptProject, 2}].enrichmentNext) {
				t.Fatal("fixture must remain before retry deadline")
			}
			next := claim(lifecycleEnrichment)
			if err := f.e.observeLifecycle(t.Context(), *next, s); err != nil {
				t.Fatal(err)
			}
			if again, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || again != nil {
				t.Fatal("new comparison wake was not consumed", again, err)
			}
		})
	}
}

func TestLifecycleFrontierRefusalConsumesDivergentSchedulerWake(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
		pull := lifecyclePull(2)
		pull.HeadSHA, pull.Title = strings.Repeat("c", 40), "unsupported title\x01"
		return pull, nil
	}
	readers := map[string]TargetReader{"test-capability": r}
	metadata, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
	if err != nil || metadata == nil {
		t.Fatal(metadata, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
		t.Fatal(err)
	}
	if lifecycleRead(t, f).HeadSHA == strings.Repeat("c", 40) {
		t.Fatal("invalid metadata retained")
	}
	admissionSQL(t, f, `INSERT INTO review_hosted_discovery_state(project_id,repository_id,pull_number,cutoff_at,initialized_at) VALUES(?,999,2,'','2026-01-01T00:00:00Z')`, attemptProject)
	if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || target == nil {
		t.Fatal(target, err)
	} else if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	due := s.due[lifecycleKey{attemptProject, 2}]
	if due.enrichmentHead != due.head || due.enrichmentBaseSHA != due.baseSHA || due.enrichmentBaseRef != due.baseRef || !due.enrichmentNext.After(clock.Now()) {
		t.Fatal("frontier refusal left scheduler wake unconsumed", due)
	}
}

func TestLifecycleHostedInventorySkipsUnchangedHealthyPolls(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	pull := lifecyclePull(2)
	r := externalReader(externalSnapshot(0, pull.HeadSHA))
	r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }
	lifecycleTick(t, f, s, r)
	for range 4 {
		clock.Add(time.Minute)
		lifecycleTick(t, f, s, r)
	}
	if r.archiveCalls != 1 || r.ciCalls.Load() != 1 {
		t.Fatalf("unchanged PR polled too early: Hosted %d, CI %d", r.archiveCalls, r.ciCalls.Load())
	}
	clock.Add(time.Minute)
	lifecycleTick(t, f, s, r)
	if r.archiveCalls != 2 || r.ciCalls.Load() != 2 {
		t.Fatalf("missing scheduled refresh: Hosted %d, CI %d", r.archiveCalls, r.ciCalls.Load())
	}
	clock.Add(time.Minute)
	pull.HeadSHA = strings.Repeat("c", 40)
	lifecycleTick(t, f, s, r)
	if r.archiveCalls != 3 || r.ciCalls.Load() != 3 {
		t.Fatalf("new head was delayed: Hosted %d, CI %d", r.archiveCalls, r.ciCalls.Load())
	}
	clock.Add(time.Minute)
	pull.BaseSHA = strings.Repeat("d", 40)
	lifecycleTick(t, f, s, r)
	if r.archiveCalls != 3 || r.ciCalls.Load() != 4 {
		t.Fatalf("base movement must refresh CI without rereading Hosted history: Hosted %d, CI %d", r.archiveCalls, r.ciCalls.Load())
	}
	clock.Add(4 * time.Minute)
	lifecycleTick(t, f, s, r)
	if r.archiveCalls != 4 {
		t.Fatalf("base movement displaced the original Hosted deadline: %d", r.archiveCalls)
	}
}

type combinedLifecycleReader struct {
	refreshTestReader
	combined func(context.Context, string, int64) (reviewprovider.Pull, reviewprovider.Repository, error)
}

func (r *combinedLifecycleReader) PullWithRepository(ctx context.Context, name string, number int64) (reviewprovider.Pull, reviewprovider.Repository, error) {
	return r.combined(ctx, name, number)
}

func TestLifecycleCombinesPullAndRepositoryWithoutTrustingReusedName(t *testing.T) {
	for _, scenario := range []string{"unchanged", "redirect", "reused_name", "throttled"} {
		t.Run(scenario, func(t *testing.T) {
			combined, identity, direct := 0, 0, 0
			pull := lifecyclePull(2)
			repo := reviewprovider.Repository{ID: 801, FullName: "owner/renamed", DefaultBranch: "main"}
			r := &combinedLifecycleReader{refreshTestReader: refreshTestReader{
				repository: func(context.Context, int64) (reviewprovider.Repository, error) { identity++; return repo, nil },
				pull: func(_ context.Context, name string, _ int64) (reviewprovider.Pull, error) {
					direct++
					if name != repo.FullName {
						t.Fatal("fallback used an unverified repository name", name)
					}
					return pull, nil
				},
			}}
			r.combined = func(_ context.Context, name string, number int64) (reviewprovider.Pull, reviewprovider.Repository, error) {
				combined++
				if name != "owner/original" || number != 2 {
					t.Fatal("combined read used wrong selector", name, number)
				}
				switch scenario {
				case "redirect":
					return reviewprovider.Pull{}, reviewprovider.Repository{}, &reviewprovider.Error{Status: 301}
				case "reused_name":
					return lifecyclePull(2), reviewprovider.Repository{ID: 802, FullName: name}, nil
				case "throttled":
					return reviewprovider.Pull{}, reviewprovider.Repository{}, &reviewprovider.Error{Code: "provider_rate_limited", Status: 429}
				default:
					return pull, repo, nil
				}
			}
			got, bound, err := readLifecyclePull(t.Context(), lifecycleTarget{reader: r, repository: 801, repositoryName: "owner/original", number: 2})
			if scenario == "throttled" {
				if err == nil || identity != 0 || direct != 0 {
					t.Fatal("throttle caused more network work", err, identity, direct)
				}
				return
			}
			wantFallback := scenario != "unchanged"
			if err != nil || !sameTarget(got, 801, 2) || bound != repo || combined != 1 || (identity == 1 && direct == 1) != wantFallback {
				t.Fatal("wrong bound observation or call count", got, bound, err, combined, identity, direct)
			}
		})
	}
}
