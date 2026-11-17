package reviewexec

import (
	"log/slog"
	"sync"
	"time"
)

type lifecycleBatchOutcome uint8

const (
	batchEligible lifecycleBatchOutcome = iota
	batchWatchFallback
	batchPrepared
	batchAccountAbort
	batchPrepareRefused
	batchPublishedMember
	batchDiscardedMember
	batchOutcomeCount
)

// Cohort and member outcomes are separate counters, not provider requests or
// green verdicts. Only opted-in batching records them. Recording never logs;
// report runs after the cohort's provider/publication work and reservation release.
type lifecycleBatchTelemetry struct {
	mu      sync.Mutex
	started time.Time
	counts  [batchOutcomeCount]uint64
}

func (t *lifecycleBatchTelemetry) record(now time.Time, outcome lifecycleBatchOutcome) {
	if outcome >= batchOutcomeCount {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.IsZero() {
		t.started = now
	}
	t.counts[outcome]++
}
func (t *lifecycleBatchTelemetry) report(now time.Time) {
	t.mu.Lock()
	if t.started.IsZero() || now.Sub(t.started) < time.Minute {
		t.mu.Unlock()
		return
	}
	start, counts := t.started, t.counts
	t.started = time.Time{}
	t.counts = [batchOutcomeCount]uint64{}
	t.mu.Unlock()
	slog.Info("Lifecycle CI batch outcomes", "window_started", start.UTC(), "window_ended", now.UTC(),
		"eligible_cohorts", counts[batchEligible], "existing_watch_fallback_cohorts", counts[batchWatchFallback],
		"prepared_cohorts", counts[batchPrepared], "account_aborted_cohorts", counts[batchAccountAbort],
		"prepare_refused_cohorts", counts[batchPrepareRefused], "published_members", counts[batchPublishedMember], "discarded_members", counts[batchDiscardedMember])
}
