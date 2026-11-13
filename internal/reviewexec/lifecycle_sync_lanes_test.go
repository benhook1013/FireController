package reviewexec

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type lifecycleLaneReader struct {
	mu        sync.Mutex
	pulls     map[int64]reviewprovider.Pull
	entered   chan reviewprovider.CITarget
	old, next chan struct{}
	ciActive  int
}

func (r *lifecycleLaneReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (r *lifecycleLaneReader) Pull(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pulls[n], nil
}
func (r *lifecycleLaneReader) ObserveRequiredCI(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	r.mu.Lock()
	r.ciActive++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.ciActive--; r.mu.Unlock() }()
	select {
	case r.entered <- target:
	case <-ctx.Done():
		return reviewprovider.CIObservation{}, ctx.Err()
	}
	gate := r.old
	if target.HeadSHA != strings.Repeat("a", 40) {
		gate = r.next
	}
	select {
	case <-gate:
		return sourceCITestObservation(target, "satisfied"), nil
	case <-ctx.Done():
		return reviewprovider.CIObservation{}, ctx.Err()
	}
}

func TestLifecycleMetadataProgressesWhileEnrichmentBlocksAndRejectsMovedHead(t *testing.T) {
	f, _, clock := lifecycleFixture(t)
	previous := int64(2)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "two-lane-targets", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
	r := &lifecycleLaneReader{pulls: map[int64]reviewprovider.Pull{2: lifecyclePull(2), 4: lifecyclePull(4)}, entered: make(chan reviewprovider.CITarget, 8), old: make(chan struct{}), next: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartLifecycleSync(ctx, map[string]TargetReader{"test-capability": r}, LifecycleSyncOptions{Now: clock.Now, ScanInterval: 5 * time.Millisecond})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var joined sync.Once
	stop := func() { joined.Do(func() { refreshStop(t, cancel, done) }) }
	t.Cleanup(stop)
	select {
	case target := <-r.entered:
		if target.PullNumber != 2 || target.HeadSHA != strings.Repeat("a", 40) {
			t.Fatal("unexpected initial enrichment", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enrichment did not start")
	}
	r.mu.Lock()
	p := r.pulls[2]
	p.HeadSHA, p.Title = strings.Repeat("c", 40), "Moved during blocked enrichment"
	r.pulls[2] = p
	p = r.pulls[4]
	p.HeadSHA, p.State, p.Merged, p.MergedAt = strings.Repeat("d", 40), "closed", true, "2026-10-21T00:00:00Z"
	r.pulls[4] = p
	r.mu.Unlock()
	clock.Add(time.Minute)
	deadline := time.After(2 * time.Second)
	for {
		var fourth reviews.PullResponse
		projectionRead(t, f, "/pulls/4", &fourth)
		if lifecycleRead(t, f).HeadSHA == strings.Repeat("c", 40) && fourth.Pull.State == "merged" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("blocked enrichment prevented head/merge observations")
		case <-time.After(5 * time.Millisecond):
		}
	}
	_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{lifecycleRead(t, f)})
	if err != nil || health[2].LastSuccessAt != observationTime(clock.Now()) || health[2].CICheckedAt != "" {
		t.Fatal("metadata health depended on unfinished enrichment", health, err)
	}
	close(r.old)
	select {
	case target := <-r.entered:
		if target.PullNumber != 2 || target.HeadSHA != strings.Repeat("c", 40) {
			t.Fatal("changed comparison did not receive independent CI collection", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cheap head marker suppressed fresh CI collection")
	}
	got := lifecycleRead(t, f)
	_, health, err = f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{got})
	if got.HeadSHA != strings.Repeat("c", 40) || got.CIState != "unknown" || health[2].CICheckedAt != "" || health[2].LastSuccessAt != observationTime(clock.Now()) || err != nil {
		t.Fatal("stale enrichment replaced current head/CI or lost metadata health", got, health, err)
	}
	stop()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ciActive != 0 {
		t.Fatal("cancellation failed to join blocked enrichment", r.ciActive)
	}
}

func TestLifecycleLanesShareCooldownAndMergeHealthFields(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}
	readers := map[string]TargetReader{"test-capability": r}
	metadata, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
	if err != nil || metadata == nil {
		t.Fatal(metadata, err)
	}
	enrichment, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || enrichment == nil {
		t.Fatal(enrichment, err)
	}
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		if another, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || another != nil {
			t.Fatal("same account/project lane admitted duplicate", lane, another, err)
		}
	}
	enrichment.due.ciCheckedAt, enrichment.due.ciError = clock.Now(), "ci_unproven"
	enrichment.due.hostedCheckedAt = clock.Now()
	s.finish(*enrichment, "open", strings.Repeat("a", 40), true, true, nil)
	metadata.due.successAt = clock.Now()
	metadata.due.baseSHA, metadata.due.baseRef = strings.Repeat("b", 40), "main"
	s.finish(*metadata, "open", strings.Repeat("a", 40), false, true, nil)
	d := s.due[lifecycleKey{attemptProject, 2}]
	if d.ciCheckedAt != clock.Now() || d.hostedCheckedAt != clock.Now() || d.successAt != clock.Now() || d.ciError != "ci_unproven" {
		t.Fatal("stale lane due snapshot lost another lane's health", d)
	}
	clock.Add(time.Minute)
	metadata, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
	if err != nil || metadata == nil {
		t.Fatal(metadata, err)
	}
	enrichment, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || enrichment == nil {
		t.Fatal(enrichment, err)
	}
	s.finish(*metadata, "open", "", false, false, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120})
	s.finish(*enrichment, "open", strings.Repeat("a", 40), false, true, nil)
	for _, account := range s.accounts {
		if account.until != clock.Now().Add(2*time.Minute) || account.lastErrorCode != "provider_rate_limited" {
			t.Fatal("successful other lane cleared a live shared cooldown", account)
		}
	}
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || target != nil {
			t.Fatal("shared cooldown did not hold both lanes", lane, target, err)
		}
	}
}
