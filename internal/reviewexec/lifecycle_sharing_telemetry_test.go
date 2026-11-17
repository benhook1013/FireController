package reviewexec

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLifecycleSharingTelemetryBoundsWindow(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	defer slog.SetDefault(old)
	var telemetry lifecycleSharingTelemetry
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 1000 {
		telemetry.record(now, shareNoEligibleOwner)
	}
	if out.Len() != 0 {
		t.Fatal("per-attempt noise")
	}
	telemetry.record(now.Add(time.Minute), shareClaimed)
	if strings.Count(out.String(), "Lifecycle CI sharing totals") != 1 || !strings.Contains(out.String(), "no_eligible_owner=1000") || !strings.Contains(out.String(), "claimed=0") {
		t.Fatal(out.String())
	}
	if telemetry.counts[shareClaimed] != 1 || telemetry.counts[shareNoEligibleOwner] != 0 {
		t.Fatal(telemetry.counts)
	}
	telemetry.record(now.Add(time.Minute), shareOutcomeCount)
	if strings.Count(out.String(), "Lifecycle CI sharing totals") != 1 {
		t.Fatal("invalid label emitted")
	}
}

func TestLifecycleSharingTelemetryNoOwnerIsNotHit(t *testing.T) {
	f, s, _, target := sharedLifecycleFixture(t)
	if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	if s.sharing.counts[shareNoEligibleOwner] != 1 || s.sharing.counts[shareClaimed] != 0 || s.sharing.counts[sharePublished] != 0 {
		t.Fatal(s.sharing.counts)
	}
}

func TestLifecycleSharingTelemetryPublishedOnlyAfterSuccess(t *testing.T) {
	f, s, _, target := sharedLifecycleFixture(t)
	out, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	if reply := receiveCIWatch(t, out); reply.err != nil || reply.value.State != "passed" {
		t.Fatal(reply)
	}
	if s.sharing.counts[shareClaimed] != 1 || s.sharing.counts[sharePublished] != 1 || s.sharing.counts[shareDiscarded] != 0 {
		t.Fatal(s.sharing.counts)
	}
}

func TestLifecycleSharingTelemetryCancellationIsNotPublished(t *testing.T) {
	f, s, _, target := sharedLifecycleFixture(t)
	out, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	ctx, stop := context.WithCancel(t.Context())
	claim := f.e.beginLifecycleCIShare(ctx, *target, s)
	if claim == nil {
		t.Fatal("claim missing")
	}
	stop()
	s.finishCIWatch(ctx, *target, claim, CIWatchItem{State: "passed"}, nil, false)
	if s.sharing.counts[shareClaimed] != 1 || s.sharing.counts[sharePublished] != 0 || s.sharing.counts[shareDiscarded] != 1 {
		t.Fatal(s.sharing.counts)
	}
	cancel()
	<-out
}

func TestLifecycleSharingTelemetrySupersededOwnerIsNotEligible(t *testing.T) {
	_, s, _, target := sharedLifecycleFixture(t)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	original := &CIWatchItem{State: "superseded", Reason: "ci_watch_source_changed"}
	entry := &ciWatchEntry{wanted: 1, inflight: true, cancel: func() {}}
	sub := &ciWatchSubscription{
		changed: make(chan struct{}, 1),
		targets: []CIWatchTarget{{PullNumber: 2, HeadSHA: head, BaseSHA: base}},
		entries: []*ciWatchEntry{entry}, minimum: []uint64{1}, bases: []string{base},
		superseded: []*CIWatchItem{original},
	}
	entry.owners = map[*ciWatchSubscription]bool{sub: true}
	claim := &ciWatchClaim{lifecycleShared: true, entry: entry, generation: 1, version: 1}
	s.finishCIWatch(t.Context(), *target, claim, CIWatchItem{State: "passed", ObservedHeadSHA: head, ObservedBaseSHA: base}, nil, false)
	if s.sharing.counts[sharePublished] != 0 || s.sharing.counts[sharePublishedWithoutEligibleOwner] != 1 {
		t.Fatal(s.sharing.counts)
	}
	if sub.superseded[0] != original {
		t.Fatal("immutable superseded evidence changed")
	}
}
