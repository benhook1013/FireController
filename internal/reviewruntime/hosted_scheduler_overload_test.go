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
