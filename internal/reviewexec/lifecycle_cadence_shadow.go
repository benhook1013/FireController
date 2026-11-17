package reviewexec

import (
	"log/slog"
	"time"
)

// Snapshot diagnostics only. This never changes deadlines or authorizes reuse.
// At most 256 existing scheduler targets are sampled per minute; target counts
// are not weighted request costs. Durable operation/intent demand is unknown.
type cadenceShadow struct {
	Total, Sampled, Watched, DirtyHint, Changed, Unhealthy, Terminal uint64
	LocallyQuiet, DurableDemandUnknown                               uint64
	Truncated, WatchIndexTruncated                                   bool
}

// Caller holds s.mu; bounded watch indexes and target scan perform no I/O.
func (s *lifecycleSchedule) cadenceShadowLocked(now time.Time) *cadenceShadow {
	if !s.shadowNext.IsZero() && now.Before(s.shadowNext) {
		return nil
	}
	s.shadowNext = now.Add(time.Minute)
	out := &cadenceShadow{Total: uint64(len(s.due)), WatchIndexTruncated: len(s.watches) > 256}
	watched := make(map[lifecycleKey]bool, min(len(s.watches), 256))
	n := 0
	for key, entry := range s.watches {
		if n == 256 {
			break
		}
		n++
		if entry != nil && len(entry.owners) > 0 {
			watched[lifecycleKey{key.binding.project, key.number}] = true
		}
	}
	for key, due := range s.due {
		if out.Sampled == 256 {
			out.Truncated = true
			break
		}
		out.Sampled++
		watch := watched[key]
		dirty := false
		for _, hint := range due.hints {
			if hint.wanted != hint.done {
				dirty = true
			}
		}
		changed := due.head == "" || due.head != due.ciHead || due.baseSHA != due.ciBaseSHA || due.baseRef != due.ciBaseRef
		unhealthy := due.lastErrorCode != "" || due.ciError != "" || due.enrichmentError != "" || due.hostedError != ""
		terminal := due.ciGateState == "success" || due.ciGateState == "failure" || due.ciGateState == "not_required"
		if watch {
			out.Watched++
		}
		if dirty {
			out.DirtyHint++
		}
		if changed {
			out.Changed++
		}
		if unhealthy {
			out.Unhealthy++
		}
		if terminal {
			out.Terminal++
		}
		// Cannot classify cold without durable demand and PR lifecycle state.
		// Watch overflow also refuses a locally quiet classification.
		if !watch && !dirty && !changed && !unhealthy && terminal && len(s.watches) <= 256 {
			out.LocallyQuiet++
		}
		out.DurableDemandUnknown++
	}
	return out
}
func logCadenceShadow(s *cadenceShadow) {
	if s == nil {
		return
	}
	slog.Info("Lifecycle cadence shadow", "targets_total", s.Total, "targets_sampled", s.Sampled, "sample_truncated", s.Truncated, "watch_index_truncated", s.WatchIndexTruncated,
		"watched", s.Watched, "dirty_hint", s.DirtyHint, "comparison_changed", s.Changed, "unhealthy", s.Unhealthy, "terminal_ci", s.Terminal,
		"locally_quiet", s.LocallyQuiet, "durable_demand_unknown", s.DurableDemandUnknown)
}

// Runs only after ordinary observation result/hold handling is finalized.
func (s *lifecycleSchedule) observeCadenceShadow() {
	if !s.options.CadenceShadow {
		return
	}
	now := s.options.Now()
	s.mu.Lock()
	snapshot := s.cadenceShadowLocked(now)
	s.mu.Unlock()
	logCadenceShadow(snapshot)
}
