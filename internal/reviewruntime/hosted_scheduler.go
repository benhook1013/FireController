package reviewruntime

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
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

func (d *HostedDriver) recoveryLoop(ctx context.Context) error {
	wait := d.wait
	if wait == nil {
		wait = waitHosted
	}
	var backoff time.Duration
	for {
		if ctx.Err() != nil {
			return nil
		}
		candidate, err := d.engine.PendingHostedRecovery(ctx, d.capability.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" {
				return err
			}
			candidate = nil
		}
		delay := hostedRecoveryInterval
		normal := candidate == nil && err == nil
		recoveryFailed := false
		if candidate != nil {
			query, queryErr := d.engine.ReadHostedAllowanceQuery(ctx, candidate.ProjectID, candidate.AttemptID)
			err = queryErr
			if queryErr == nil {
				actor := d.providerActor()
				if query.ProviderActor.ID != actor.ID || query.ProviderActor.Kind != actor.Kind {
					err = core.Fail("review_conflict", "The original hosted provider identity does not match this recovery capability.", 409)
				} else {
					// RecoverHosted accepts a read-only interface. Original posting identity
					// survives credential revocation, policy pauses and a changed login actor.
					var observed RecoveredHosted
					observed, err = RecoverHosted(ctx, d.provider, d.captures, d.engine, candidate.ProjectID, candidate.AttemptID, d.capability.ID, query.PostingActor, actor)
					// A reason is returned only after the original recovery binding
					// was read successfully; binding refusals keep ordinary backoff.
					recoveryFailed = err != nil && observed.Reason != ""
					// Healthy pending work keeps a short cadence. Identical provider
					// source reuses its retained capture rather than spending another
					// journal reference. Ambiguous outcomes/errors still back off.
					normal = err == nil && observed.State == "completed"
					if err == nil && observed.State == "active" && (observed.Reason == "publication_not_proven" || observed.Reason == "missing_publication_batches") {
						normal = true
						delay = hostedRecoveryBackoffMinimum
					}
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if normal {
			backoff = 0
		} else {
			if backoff < hostedRecoveryBackoffMinimum {
				backoff = hostedRecoveryBackoffMinimum
			} else {
				backoff = min(backoff*2, hostedRecoveryBackoffMaximum)
			}
			delay = backoff
			var providerError *reviewprovider.Error
			hasProviderError := errors.As(err, &providerError)
			rateLimited := hasProviderError && (providerError.Code == "provider_rate_limited" || providerError.Status == 429)
			// An issued result may already be captured while local publication or
			// settlement failed. Retry this same pass promptly, retaining semantic
			// holds and provider cooldowns rather than starting another observer.
			if recoveryFailed && !rateLimited {
				delay = min(delay, hostedRecoveryPendingErrorMaximum)
			}
			if hasProviderError && providerError.RetryAfterSeconds > 0 {
				retry := time.Duration(min(providerError.RetryAfterSeconds, int64(hostedRecoveryRetryMaximum/time.Second))) * time.Second
				delay = max(delay, retry)
			}
		}
		if err := wait(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
