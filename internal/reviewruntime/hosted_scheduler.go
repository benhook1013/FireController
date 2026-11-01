package reviewruntime

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const hostedRecoveryInterval = 15 * time.Second
const hostedRecoveryBackoffMinimum = 30 * time.Second
const hostedRecoveryBackoffMaximum = 5 * time.Minute
const hostedRecoveryPendingErrorMaximum = time.Minute
const hostedRecoveryRetryMaximum = 24 * time.Hour

// StartRecovery observes only already-issued hosted work. The application starts
// it after the engine's interrupted-attempt recovery, and owns cancellation and
// the single terminal result. No recovery outcome authorizes another post.
func (d *HostedDriver) StartRecovery(ctx context.Context) (<-chan error, error) {
	if ctx == nil || d == nil || d.engine == nil || d.captures == nil || d.provider == nil || !validIdentifier(d.capability.ID) || !validHostedSettings(d.capability.Hosted) || !d.recovering.CompareAndSwap(false, true) {
		return nil, core.Fail("validation_failed", "Start one recovery worker for the configured hosted capability.", 400)
	}
	done := make(chan error, 1)
	go func() {
		err := d.recoveryLoop(ctx)
		d.recovering.Store(false)
		done <- err
		close(done)
	}()
	return done, nil
}

type hostedRecoverySlot struct {
	due     time.Time
	backoff time.Duration
	check   *reviewexec.HostedObserverCheck
}

func (d *HostedDriver) recoveryLoop(ctx context.Context) (failure error) {
	slots := map[string]*hostedRecoverySlot{}
	defer func() {
		for _, slot := range slots {
			d.engine.StopHostedObserver(slot.check, failure)
		}
	}()
	wait := d.wait
	if wait == nil {
		wait = waitHosted
	}
	now := time.Now()
	var selectionBackoff time.Duration
	cursor := ""
	pause := func(delay time.Duration) error {
		if err := wait(ctx, delay); err != nil {
			return err
		}
		// A successful wait completed its requested interval. This also lets the
		// existing trusted wait seam advance scheduling without wall-clock sleeps.
		now = now.Add(delay)
		if actual := time.Now(); actual.After(now) {
			now = actual
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		candidates, err := d.engine.PendingHostedRecoveries(ctx, d.capability.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var local *core.Error
			if !errors.As(err, &local) || local.Code != "overloaded" {
				return err
			}
			selectionBackoff = min(max(selectionBackoff*2, hostedRecoveryBackoffMinimum), hostedRecoveryBackoffMaximum)
			if err = pause(selectionBackoff); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			continue
		}
		selectionBackoff = 0
		current := map[string]bool{}
		for _, candidate := range candidates {
			current[candidate.AttemptID] = true
			if slots[candidate.AttemptID] == nil {
				slots[candidate.AttemptID] = &hostedRecoverySlot{}
			}
		}
		for id, slot := range slots {
			if !current[id] {
				d.engine.ForgetHostedObserver(slot.check)
				delete(slots, id)
			}
		}
		accountDue := d.engine.HostedRecoveryNotBefore(d.capability.ID)
		var selected *reviewexec.HostedRecoveryCandidate
		if !accountDue.After(now) {
			// Candidates are stable-sorted by attempt ID; wrap after the last served
			// target rather than repeatedly selecting a slow oldest repository.
			for pass := 0; pass < 2 && selected == nil; pass++ {
				for i := range candidates {
					c := &candidates[i]
					if (pass == 0 && c.AttemptID <= cursor) || (pass == 1 && c.AttemptID > cursor) {
						continue
					}
					if !slots[c.AttemptID].due.After(now) {
						selected = c
						break
					}
				}
			}
		}
		if selected == nil {
			delay := hostedRecoveryInterval
			if accountDue.After(now) {
				delay = accountDue.Sub(now)
			} else {
				for _, slot := range slots {
					if slot.due.After(now) {
						delay = min(delay, slot.due.Sub(now))
					}
				}
			}
			if err = pause(delay); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			continue
		}
		candidate := *selected
		cursor = candidate.AttemptID
		slot := slots[candidate.AttemptID]
		slot.check = d.engine.BeginHostedObserver(d.capability.ID, candidate.ProjectID, candidate.OperationID, candidate.AttemptID)
		reason := "observation_unavailable"
		query, queryErr := d.engine.ReadHostedAllowanceQuery(ctx, candidate.ProjectID, candidate.AttemptID)
		err = queryErr
		delay, normal, ended, recoveryFailed := hostedRecoveryInterval, false, false, false
		if queryErr == nil {
			actor := d.providerActor()
			if query.ProviderActor.ID != actor.ID || query.ProviderActor.Kind != actor.Kind {
				err = core.Fail("review_conflict", "The original hosted provider identity does not match this recovery capability.", 409)
			} else {
				var observed RecoveredHosted
				observed, err = RecoverHosted(ctx, d.provider, d.captures, d.engine, candidate.ProjectID, candidate.AttemptID, d.capability.ID, query.PostingActor, actor)
				reason = observed.Reason
				recoveryFailed = err != nil && observed.Reason != ""
				ended = err == nil && (observed.State == "completed" || observed.State == "rate_limited")
				normal = ended
				if err == nil && observed.State == "active" && (observed.Reason == "publication_not_proven" || observed.Reason == "missing_publication_batches" || observed.Reason == "attribution_window_closed" && observed.TriggerCommentID > 0) {
					normal, delay = true, hostedRecoveryBackoffMinimum
				}
			}
		}
		d.engine.FinishHostedObserver(slot.check, time.Now().UTC(), time.Time{}, reason, hostedObserverFailure(reason, err))
		if ctx.Err() != nil {
			return nil
		}
		if actual := time.Now(); actual.After(now) {
			now = actual
		}
		if normal {
			slot.backoff = 0
		} else {
			slot.backoff = min(max(slot.backoff*2, hostedRecoveryBackoffMinimum), hostedRecoveryBackoffMaximum)
			delay = slot.backoff
			var provider *reviewprovider.Error
			if errors.As(err, &provider) {
				rateLimited := provider.Code == "provider_rate_limited" || provider.Status == 429
				if recoveryFailed && !rateLimited {
					delay = min(delay, hostedRecoveryPendingErrorMaximum)
				}
				if provider.RetryAfterSeconds > 0 {
					retry := time.Duration(min(provider.RetryAfterSeconds, int64(hostedRecoveryRetryMaximum/time.Second))) * time.Second
					delay = max(delay, retry)
					d.engine.DelayHostedRecoveryAccount(d.capability.ID, now.Add(delay))
				} else if rateLimited {
					d.engine.DelayHostedRecoveryAccount(d.capability.ID, now.Add(delay))
				}
			} else if recoveryFailed {
				delay = min(delay, hostedRecoveryPendingErrorMaximum)
			}
		}
		slot.due = now.Add(delay)
		d.engine.ScheduleHostedObserver(slot.check, slot.due)
		if ended {
			d.engine.ForgetHostedObserver(slot.check)
			delete(slots, candidate.AttemptID)
		}
	}
}
