package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func lifecycleErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var failure *reviewprovider.Error
	if errors.As(err, &failure) && identifier.MatchString(failure.Code) {
		return failure.Code
	}
	return "provider_unavailable"
}
func observationTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// ObservationHealth reads current configuration and bounded process-local
// scheduler state only. Successful unchanged polls update this state without
// snapshots/events or provider calls from the read path. A restart starts fresh.
func (e *Engine) ObservationHealth(ctx context.Context, project string, pulls []reviews.PullSummary) (reviews.ObserverHealth, map[int64]reviews.PullObservationHealth, error) {
	out := reviews.ObserverHealth{State: "unconfigured"}
	items := make(map[int64]reviews.PullObservationHealth, len(pulls))
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || len(pulls) > 100 {
		return out, items, invalid()
	}
	var c Configuration
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		c, _, err = readConfiguration(ctx, tx, project)
		return err
	})
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
		return out, items, nil
	}
	if err != nil {
		return out, items, err
	}
	out.State = "unavailable"
	e.lifecycleMu.RLock()
	s := e.lifecycleSchedule
	e.lifecycleMu.RUnlock()
	running := e.lifecycleRunning.Load() && s != nil && e.validateConfiguration(c) == nil
	if running {
		out.State = "running"
		out.StartedAt = observationTime(s.startedAt)
	}
	for _, pull := range pulls {
		h := reviews.PullObservationHealth{State: "not_enrolled"}
		if !slices.Contains(c.Queue, pull.Number) {
			items[pull.Number] = h
			continue
		}
		h.State = "unavailable"
		if running {
			s.mu.Lock()
			due := s.due[lifecycleKey{project, pull.Number}]
			account := s.accounts[e.capabilities[c.CapabilityID].Provider+"\x00"+e.capabilities[c.CapabilityID].Account]
			s.mu.Unlock()
			now := s.options.Now()
			h = reviews.PullObservationHealth{State: "waiting", CheckedAt: observationTime(due.checkedAt), LastSuccessAt: observationTime(due.successAt), NextCheckAt: observationTime(due.next), Error: due.lastErrorCode, CICheckedAt: observationTime(due.ciCheckedAt), CIError: due.ciError, HostedCheckedAt: observationTime(due.hostedCheckedAt), HostedError: due.hostedError}
			if !due.successAt.IsZero() {
				h.State = "current"
			}
			if h.State == "current" && (due.head != pull.HeadSHA || due.baseSHA != pull.BaseSHA || due.baseRef != pull.BaseRef) {
				h.State = "comparison_changed"
			}
			if !due.next.IsZero() && now.After(due.next.Add(30*time.Second)) {
				h.State = "overdue"
			}
			if h.Error != "" {
				h.State = "retrying"
			}
			if now.Before(account.until) {
				h.State = "retrying"
				h.Error = account.lastErrorCode
				h.NextCheckAt = observationTime(maxObservationTime(due.next, account.until))
			}
		}
		items[pull.Number] = h
	}
	return out, items, nil
}
func maxObservationTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
