package reviewexec

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

type cadenceDemand struct {
	targets  map[lifecycleKey]bool
	projects map[string]bool
	complete bool
}

// Two indexed bounded scans, one read-only snapshot. Overflow or any read error
// discards absence claims. Late-bound intents conservatively demand the project.
func readCadenceDemand(ctx context.Context, tx *sql.Tx) cadenceDemand {
	d := cadenceDemand{targets: map[lifecycleKey]bool{}, projects: map[string]bool{}}
	for _, query := range []string{
		`SELECT project_id,pull_number FROM review_operations WHERE state IN ('queued','preparing','posting','active','uncertain') LIMIT 257`,
		`SELECT project_id,expected_pull_number FROM review_selections WHERE state IN ('queued','selecting') LIMIT 257`,
	} {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return d
		}
		count := 0
		for rows.Next() {
			count++
			var project string
			var number int64
			if err = rows.Scan(&project, &number); err != nil || project == "" || number < 0 || count > 256 {
				rows.Close()
				return d
			}
			if number == 0 {
				d.projects[project] = true
			} else {
				d.targets[lifecycleKey{project, number}] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return d
		}
	}
	d.complete = true
	return d
}

type cadenceDemandSummary struct {
	Sampled, Demanded, UnboundProject, LocalQuietNoDurableDemand, Unknown uint64
	Truncated                                                             bool
}

// This join is diagnostic evidence from successive snapshots, not atomic
// admission authority. Caller holds the schedule lock. No identifiers escape.
func (s *lifecycleSchedule) joinCadenceDemandLocked(d cadenceDemand) cadenceDemandSummary {
	out := cadenceDemandSummary{}
	for key, due := range s.due {
		if out.Sampled == 256 {
			out.Truncated = true
			break
		}
		out.Sampled++
		if !d.complete {
			out.Unknown++
			continue
		}
		if d.projects[key.project] {
			out.UnboundProject++
			out.Demanded++
			continue
		}
		if d.targets[key] {
			out.Demanded++
			continue
		}
		watched := false
		if len(s.watches) > 256 {
			out.Unknown++
			continue
		}
		for wk, entry := range s.watches {
			if wk.binding.project == key.project && wk.number == key.number && entry != nil && len(entry.owners) > 0 {
				watched = true
				break
			}
		}
		dirty := false
		for _, h := range due.hints {
			if h.wanted != h.done {
				dirty = true
			}
		}
		unchanged := due.head != "" && due.head == due.ciHead && due.baseSHA == due.ciBaseSHA && due.baseRef == due.ciBaseRef
		healthy := due.lastErrorCode == "" && due.ciError == "" && due.enrichmentError == "" && due.hostedError == ""
		terminal := due.ciGateState == "success" || due.ciGateState == "failure" || due.ciGateState == "not_required"
		if !watched && !dirty && unchanged && healthy && terminal {
			out.LocalQuietNoDurableDemand++
		}
	}
	return out
}

// Best-effort once/minute after ordinary collection. The 100ms read budget is
// isolated from observation results; no provider work or scheduling writes.
func (e *Engine) observeCadenceDemand(ctx context.Context, s *lifecycleSchedule) {
	if ctx.Err() != nil {
		return
	}
	now := s.options.Now()
	s.mu.Lock()
	if !s.demandShadowNext.IsZero() && now.Before(s.demandShadowNext) {
		s.mu.Unlock()
		return
	}
	s.demandShadowNext = now.Add(time.Minute)
	s.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	d := cadenceDemand{}
	err := e.readAdmission(bounded, func(ctx context.Context, tx *sql.Tx) error { d = readCadenceDemand(ctx, tx); return nil })
	if err != nil || bounded.Err() != nil {
		d.complete = false
	}
	s.mu.Lock()
	summary := s.joinCadenceDemandLocked(d)
	s.mu.Unlock()
	slog.Info("Lifecycle durable demand shadow", "targets_sampled", summary.Sampled, "sample_truncated", summary.Truncated,
		"durable_demanded", summary.Demanded, "unbound_project_demand", summary.UnboundProject,
		"locally_quiet_without_durable_demand", summary.LocalQuietNoDurableDemand, "demand_unknown", summary.Unknown,
		"read_us", time.Since(started).Microseconds(), "complete", d.complete)
}
