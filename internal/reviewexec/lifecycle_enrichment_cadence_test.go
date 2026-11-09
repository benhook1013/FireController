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
