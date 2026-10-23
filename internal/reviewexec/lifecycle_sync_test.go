package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type lifecycleTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lifecycleTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *lifecycleTestClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func lifecycleFixture(t *testing.T) (*attemptFixture, *lifecycleSchedule, *lifecycleTestClock) {
	t.Helper()
	f := attemptProjectionFixture(t, true)
	previous := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "paused-lifecycle", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
	clock := &lifecycleTestClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	c, _ := lifecycleOptions([]LifecycleSyncOptions{{Now: clock.Now}})
	return f, newLifecycleSchedule(c), clock
}
func lifecyclePull(n int64) reviewprovider.Pull {
	return reviewprovider.Pull{Number: n, RepositoryID: 801, Title: fmt.Sprintf("Synthetic PR %d", n), State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 0, Mergeability: "mergeable"}
}
func lifecycleTick(t *testing.T, f *attemptFixture, s *lifecycleSchedule, r TargetReader) bool {
	t.Helper()
	metadata := false
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		target, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lane)
		if err != nil {
			t.Fatal(err)
		}
		if target != nil {
			metadata = metadata || lane == lifecycleMetadata
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	return metadata
}
func lifecycleRead(t *testing.T, f *attemptFixture) reviews.PullSummary {
	t.Helper()
	var got reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &got)
	return got.Pull
}

func TestLifecycleSyncPausedNoopHeadCloseMergeAndReopen(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	pull := lifecyclePull(2)
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }}}
	before := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
	if !lifecycleTick(t, f, s, r) {
		t.Fatal("no automatic enrolled target")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != before {
		t.Fatal("unchanged source created snapshot")
	}
	if lifecycleTick(t, f, s, r) {
		t.Fatal("poll ignored cadence")
	}
	clock.Add(time.Minute)
	if !lifecycleTick(t, f, s, r) || r.ciCalls.Load() != 1 {
		t.Fatal("cheap poll repeated CI")
	}
	clock.Add(time.Minute)
	pull.HeadSHA = strings.Repeat("c", 40)
	pull.Title = "Rebased while review dispatch paused"
	r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return reviewprovider.CIObservation{}, errors.New("PRIVATE provider failure")
	}
	if !lifecycleTick(t, f, s, r) || r.ciCalls.Load() != 2 {
		t.Fatal("head change did not trigger fresh CI before its ordinary interval")
	}
	got := lifecycleRead(t, f)
	if got.HeadSHA != pull.HeadSHA || got.Title != pull.Title || got.CIState != "unknown" || got.Complete {
		t.Fatal("old head credit inherited", got)
	}
	clock.Add(time.Minute)
	pull.State = "closed"
	lifecycleTick(t, f, s, r)
	if lifecycleRead(t, f).State != "closed" {
		t.Fatal("close not observed")
	}
	clock.Add(time.Minute)
	if lifecycleTick(t, f, s, r) {
		t.Fatal("closed poll too frequent")
	}
	clock.Add(4 * time.Minute)
	pull.State = "open"
	r.ci = nil
	if !lifecycleTick(t, f, s, r) || lifecycleRead(t, f).State != "open" {
		t.Fatal("reopen not observed")
	}
	clock.Add(time.Minute)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = "2026-10-21T00:00:00Z"
	lifecycleTick(t, f, s, r)
	if got = lifecycleRead(t, f); got.State != "merged" || got.MergedAt != pull.MergedAt {
		t.Fatal("merge time lost", got)
	}
	clock.Add(time.Hour)
	if lifecycleTick(t, f, s, r) {
		t.Fatal("merged rows polled too frequently")
	}
	clock.Add(5 * time.Hour)
	before = attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
	lifecycleTick(t, f, s, r)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != before {
		t.Fatal("unchanged merge created event")
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_source_refreshes"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("automatic observation created action", table)
		}
	}
	if r.calls.Load()%2 != 0 {
		t.Fatal("unexpected read fanout")
	}
}

func TestLifecycleSyncCIUnknownNotRequiredAndComparisonChanges(t *testing.T) {
	for _, state := range []string{"satisfied", "failed", "pending", "not_required", "unproven"} {
		t.Run(state, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}, ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(target, state)
				if state == "unproven" {
					o.Reason = "ci_unknown"
				}
				return o, nil
			}}
			lifecycleTick(t, f, s, r)
			want := map[string]string{"satisfied": "success", "failed": "failure", "pending": "pending", "not_required": "unknown", "unproven": "unknown"}[state]
			if got := lifecycleRead(t, f); got.CIState != want {
				t.Fatal("CI projection", state, got.CIState)
			}
		})
	}
}

func TestLifecycleSyncAccountCooldownAndConfigurationRetirementHolds(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	r := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120}
	}, pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}
	lifecycleTick(t, f, s, r)
	clock.Add(time.Minute)
	if lifecycleTick(t, f, s, r) || r.calls.Load() != 1 {
		t.Fatal("rate-limit cooldown ignored")
	}
	clock.Add(time.Minute)
	r.repository = nil
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	// Retirement after pinning must prevent both I/O and implicit reactivation.
	previous := int64(1)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, "retire-observed", encoded(reviews.QueueEditRequest{ExpectedRevision: &previous, RepositoryID: 801, Source: "fixture", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}}))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	before := r.calls.Load()
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	if r.calls.Load() != before {
		t.Fatal("retired target fetched")
	}
	if delay := lifecycleProviderDelay(&reviewprovider.Error{RetryAfterSeconds: 1 << 62}, time.Minute); delay != 24*time.Hour {
		t.Fatal("unbounded retry", delay)
	}
}

func TestLifecycleSyncCompletionContentionKeepsFetchedResult(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.Pull{}, ctx.Err()
		}
		p := lifecyclePull(n)
		p.Title = "Fetched once"
		return p, nil
	}}
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *target, s) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not entered")
	}
	lock := refreshContentionLock(t, f)
	close(release)
	select {
	case err := <-done:
		t.Fatal("local writer contention discarded fetched result", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completion did not recover")
	}
	if r.calls.Load() != 2 || lifecycleRead(t, f).Title != "Fetched once" {
		t.Fatal("provider repeated or result lost", r.calls.Load())
	}
}

func TestLifecycleSyncCorruptMetadataRefusesPublication(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	r := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	if _, err = f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json='{"is_draft":"corrupt"}' WHERE project_id=? AND number=2`, attemptProject); err != nil {
		t.Fatal(err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err == nil {
		t.Fatal("corrupt metadata concealed by CAS skip")
	}
}

func TestLifecycleSyncConcurrentConfigurationChangeHoldsFetchedResult(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	r := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		previous := int64(2)
		_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "changed-enrollment", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli", "hosted"}, Paused: true})
		if err != nil {
			t.Error(err)
		}
		p := lifecyclePull(2)
		p.Title = "Old in-flight source"
		return p, nil
	}}
	before := lifecycleRead(t, f)
	lifecycleTick(t, f, s, r)
	if after := lifecycleRead(t, f); after.Title != before.Title {
		t.Fatal("configuration changed but result published", after.Title)
	}
}
