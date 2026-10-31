package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestObservationHealthUnchangedPollFailureRecoveryAndRestart(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	f.e.lifecycleSchedule = s
	f.e.lifecycleRunning.Store(true)
	pull := lifecyclePull(2)
	fail := false
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		if fail {
			return reviewprovider.Pull{}, errors.New("PRIVATE provider failure")
		}
		return pull, nil
	}}}
	read := func() reviews.PullObservationHealth {
		t.Helper()
		observer, items, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{lifecycleRead(t, f)})
		if err != nil || observer.State != "running" || observer.StartedAt == "" {
			t.Fatal(observer, err)
		}
		return items[2]
	}
	if got := read(); got.State != "waiting" || got.LastSuccessAt != "" {
		t.Fatal(got)
	}
	snapshots := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	if !lifecycleTick(t, f, s, r) {
		t.Fatal("no poll")
	}
	first := read()
	if first.State != "current" || first.LastSuccessAt != observationTime(clock.Now()) || first.CICheckedAt == "" {
		t.Fatal(first)
	}
	clock.Add(time.Minute)
	lifecycleTick(t, f, s, r)
	second := read()
	if second.LastSuccessAt == first.LastSuccessAt || second.CICheckedAt != first.CICheckedAt {
		t.Fatal(first, second)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != snapshots || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("unchanged polls churned durable history")
	}
	fail = true
	clock.Add(time.Minute)
	lifecycleTick(t, f, s, r)
	held := read()
	if held.State != "retrying" || held.Error != "provider_unavailable" || held.LastSuccessAt != second.LastSuccessAt {
		t.Fatal(held)
	}
	fail = false
	clock.Add(time.Minute)
	lifecycleTick(t, f, s, r)
	if got := read(); got.State != "current" || got.Error != "" {
		t.Fatal(got)
	}
	clock.Add(2 * time.Minute)
	if got := read(); got.State != "overdue" {
		t.Fatal(got)
	}
	f.e.lifecycleRunning.Store(false)
	observer, items, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{lifecycleRead(t, f)})
	if err != nil || observer.State != "unavailable" || items[2].State != "unavailable" {
		t.Fatal(observer, items, err)
	}
	f.e.lifecycleSchedule = newLifecycleSchedule(s.options)
	f.e.lifecycleRunning.Store(true)
	if got := read(); got.State != "waiting" || got.LastSuccessAt != "" {
		t.Fatal("restart claimed old check", got)
	}
}

func TestObservationHealthSeparatesCIHostedFailureAndSkippedCollection(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	f.e.lifecycleSchedule = s
	f.e.lifecycleRunning.Store(true)
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	ciErr := error(&reviewprovider.Error{Code: "provider_invalid_response"})
	r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return reviewprovider.CIObservation{}, ciErr
	}
	read := func() reviews.PullObservationHealth {
		t.Helper()
		_, items, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{lifecycleRead(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		return items[2]
	}
	lifecycleTick(t, f, s, r)
	first := read()
	if first.CIError != "ci_provider_invalid_response" || first.HostedError != "" || first.HostedCheckedAt == "" || first.LastSuccessAt == "" || r.archiveCalls != 1 {
		t.Fatal("independent successful Hosted inventory was hidden by CI failure", first)
	}
	for _, account := range s.accounts {
		if account.backoff != 0 || !account.until.IsZero() {
			t.Fatal("one malformed CI inventory delayed unrelated account targets", account)
		}
	}
	clock.Add(5 * time.Minute)
	ciErr = &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120}
	lifecycleTick(t, f, s, r)
	limited := read()
	if limited.CIError != "ci_provider_rate_limited" || limited.HostedCheckedAt != first.HostedCheckedAt || limited.HostedError != first.HostedError || r.archiveCalls != 1 {
		t.Fatal("skipped Hosted read fabricated a new check", first, limited)
	}
	clock.Add(5 * time.Minute)
	ciErr = nil
	r.ci = nil
	r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
		return reviewprovider.HostedPublicationSnapshot{}, &reviewprovider.Error{Code: "provider_invalid_response"}
	}
	lifecycleTick(t, f, s, r)
	hosted := read()
	if hosted.HostedError != "provider_invalid_response" || hosted.HostedCheckedAt == first.HostedCheckedAt || hosted.CIError != "" {
		t.Fatal("Hosted failure did not retain its independent diagnostic", hosted)
	}
	for _, account := range s.accounts {
		if account.backoff != 0 || !account.until.IsZero() {
			t.Fatal("malformed Hosted inventory delayed unrelated account targets", account)
		}
	}
	if delay := lifecycleProviderDelay(&reviewprovider.Error{Code: "provider_invalid_response", RetryAfterSeconds: 120}, 0); delay != 2*time.Minute {
		t.Fatal("explicit provider cooldown ignored", delay)
	}
}

func TestObservationHealthEnrichmentPreCollectionFailurePreservesMetadataEvidence(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	f.e.lifecycleSchedule = s
	f.e.lifecycleRunning.Store(true)
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}
	readers := map[string]TargetReader{"test-capability": r}
	tick := func(lane lifecycleLane) {
		t.Helper()
		target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
		if err != nil || target == nil {
			t.Fatal("missing eligible lane", lane, target, err)
		}
		if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
			t.Fatal(err)
		}
	}
	read := func() reviews.PullObservationHealth {
		t.Helper()
		_, items, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{lifecycleRead(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		return items[2]
	}
	tick(lifecycleMetadata)
	metadata := read()
	clock.Add(5 * time.Second)
	r.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, &reviewprovider.Error{Code: "provider_unavailable", Message: "PRIVATE provider details"}
	}
	tick(lifecycleEnrichment)
	failed := read()
	if failed.State != "retrying" || failed.Error != "provider_unavailable" || failed.CheckedAt != metadata.CheckedAt || failed.LastSuccessAt != metadata.LastSuccessAt || failed.CICheckedAt != "" || failed.HostedCheckedAt != "" || r.ciCalls.Load() != 0 {
		t.Fatal("pre-collection failure hidden or metadata evidence replaced", metadata, failed)
	}
	clock.Add(time.Minute)
	r.repository = nil
	tick(lifecycleMetadata)
	metadata = read()
	if metadata.Error != "provider_unavailable" || metadata.LastSuccessAt != observationTime(clock.Now()) {
		t.Fatal("metadata success hid unresolved enrichment failure", metadata)
	}
	tick(lifecycleEnrichment)
	recovered := read()
	if recovered.State != "current" || recovered.Error != "" || recovered.CheckedAt != metadata.CheckedAt || recovered.LastSuccessAt != metadata.LastSuccessAt || recovered.CICheckedAt == "" {
		t.Fatal("enrichment recovery changed metadata evidence or retained stale failure", metadata, recovered)
	}
}
