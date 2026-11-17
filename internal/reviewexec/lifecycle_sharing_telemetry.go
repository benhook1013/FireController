package reviewexec

import (
	"log/slog"
	"sync"
	"time"
)

type lifecycleShareOutcome uint8

const (
	shareNotDue lifecycleShareOutcome = iota
	shareNoEligibleOwner
	shareDemandChanged
	shareClaimed
	sharePublished
	sharePublishedWithoutEligibleOwner
	shareDiscarded
	shareOutcomeCount
)

// Fixed counters contain no target, account, credential or error text. Windows
// flush on the next ordinary observation, never via an extra provider request.
type lifecycleSharingTelemetry struct {
	mu      sync.Mutex
	started time.Time
	counts  [shareOutcomeCount]uint64
}

func (t *lifecycleSharingTelemetry) record(now time.Time, outcome lifecycleShareOutcome) {
	if outcome >= shareOutcomeCount {
		return
	}
	t.mu.Lock()
	if t.started.IsZero() {
		t.started = now
	}
	var completed [shareOutcomeCount]uint64
	start := t.started
	flush := now.Sub(start) >= time.Minute
	if flush {
		completed = t.counts
		t.counts = [shareOutcomeCount]uint64{}
		t.started = now
	}
	t.counts[outcome]++
	t.mu.Unlock()
	if flush {
		slog.Info("Lifecycle CI sharing totals", "window_started", start.UTC(), "window_ended", now.UTC(),
			"not_due", completed[shareNotDue], "no_eligible_owner", completed[shareNoEligibleOwner],
			"demand_changed", completed[shareDemandChanged], "claimed", completed[shareClaimed],
			"published_for_eligible_owner", completed[sharePublished], "published_without_eligible_owner", completed[sharePublishedWithoutEligibleOwner],
			"discarded", completed[shareDiscarded])
	}
}
