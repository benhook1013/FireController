package reviewruntime

import (
	"context"
	"testing"
	"time"
)

func TestHostedSchedulerCandidateOverloadBacksOffAndRecovers(t *testing.T) {
	f := newHostedDriverFixture(t)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust the actual read pool until its own two-second budget expires,
	// leaving the owning recovery context alive.
	gate.interval(t, 30*time.Second)
	if ctx.Err() != nil || !f.hosted.recovering.Load() {
		t.Fatal("transient read timeout stopped owner")
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("selection retry issued provider work")
	}
	cancel()
	schedulerDone(t, done, false)
}

func TestHostedSchedulerRetainsNextCheckAfterCandidateOverload(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	f.provider.partial = true
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	first := readHostedObserver(t, f, a)
	observations := f.provider.observations
	repositories := p.repositories.Load()

	// Exhaust the real pool while the scheduler selects its next candidate.
	// Its bounded admission read returns overloaded without a provider read.
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := time.Now().UTC()
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	second := readHostedObserver(t, f, a)
	next, err := time.Parse(time.RFC3339Nano, second.NextCheckAt)
	if err != nil || next.Before(before.Add(30*time.Second)) || second.NextCheckAt == first.NextCheckAt {
		t.Fatalf("overloaded candidate selection kept stale next check: first=%+v second=%+v error=%v", first, second, err)
	}
	if second.LastCheckAt != first.LastCheckAt || second.LastSourceAt != first.LastSourceAt || second.State != first.State || second.Reason != first.Reason || second.Error != first.Error {
		t.Fatal("candidate overload invented a completed observation", first, second)
	}
	if f.provider.observations != observations || p.repositories.Load() != repositories || f.provider.queries != 1 || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("candidate overload changed provider work or review accounting")
	}
	cancel()
	schedulerDone(t, done, false)
}
