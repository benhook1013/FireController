package reviewexec

import (
	"context"
	"errors"
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
