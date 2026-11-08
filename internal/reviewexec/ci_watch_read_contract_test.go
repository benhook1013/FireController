package reviewexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestCIWatchZeroWaitDoesNotReuseSharedObservedFacts(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	target := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := s.registerCIWatch(scope, binding, []CIWatchTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	defer s.unregisterCIWatch(retained)
	reader.states[42] = "satisfied"
	watchTick(t, f, s)
	if s.ciWatchResult(retained).State != "passed" {
		t.Fatal("fixture has no observed passing CI")
	}
	calls := reader.calls
	// No new observer claim is allowed to run during this call. A new watch
	// cannot reuse even the passing facts already present in its shared entry.
	got, err := f.e.WatchCI(t.Context(), scope, []CIWatchTarget{target}, 0)
	if err != nil || got.State != "pending" || !got.TimedOut || len(got.Targets) != 1 {
		t.Fatal(got, err)
	}
	item := got.Targets[0]
	if item.Reason != "ci_watch_check_pending" || item.ObservedAt != "" || item.ObservedHeadSHA != "" || item.ObservedBaseSHA != "" || item.ObservationKey != "" || item.Required != 0 || len(item.Checks) != 0 {
		t.Fatal("cached observation settled a fresh zero wait", got)
	}
	if reader.calls != calls || len(s.watchers) != 1 {
		t.Fatal("zero wait started provider work or retained subscription", reader.calls, len(s.watchers))
	}
}

func TestCIWatchMissingCapabilityIsNotReadAdmissionRetry(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	delete(f.e.capabilities, "test-capability")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	err := core.RetryReadAdmission(ctx, time.Now().Add(900*time.Second), func(ctx context.Context) error {
		calls++
		_, err := f.e.ciWatchBinding(ctx, scope)
		return err
	})
	if calls != 1 || ctx.Err() != nil || core.IsReadAdmissionOverload(err) || ciWatchDiagnosticReason(t, err) != "ci_watch_observer_unavailable" {
		t.Fatal("missing capability retried as admission pressure", calls, err, ctx.Err())
	}
	_, err = f.e.WatchCI(ctx, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}, 900)
	if ctx.Err() != nil || ciWatchDiagnosticReason(t, err) != "ci_watch_observer_unavailable" || reader.calls != 0 || len(s.watchers) != 0 {
		t.Fatal("missing capability waited or acquired work", err, ctx.Err())
	}
}
