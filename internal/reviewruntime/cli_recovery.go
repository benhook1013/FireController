package reviewruntime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

// StartCLIRecovery owns local verification, publication and settlement of
// retained CLI results. No code path here launches a process or contacts a provider.
func (d *CLIDriver) StartCLIRecovery(ctx context.Context) (<-chan error, error) {
	if ctx == nil || d == nil || d.engine == nil || d.captures == nil || d.manager == nil || !d.recovering.CompareAndSwap(false, true) {
		return nil, core.Fail("validation_failed", "Start one retained CLI recovery worker for the configured capability.", 400)
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer d.recovering.Store(false)
		var backoff time.Duration
		wait := d.recoveryWait
		if wait == nil {
			wait = waitPreparation
		}
		for {
			if ctx.Err() != nil {
				done <- nil
				return
			}
			settleErr := d.settleRestartedCLI(ctx)
			rawErr := errors.Join(settleErr, d.recoverRawCLI(ctx))
			// A malformed ended result must not stop another complete result.
			err := errors.Join(rawErr, d.engine.RecoverRetainedCLI(ctx, d.capability.ID))
			if ctx.Err() != nil {
				done <- nil
				return
			}
			delay := 15 * time.Second
			if err != nil {
				var failure *core.Error
				if rawErr == nil && !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &failure) || failure.Code != "overloaded" && failure.Code != "review_conflict") {
					done <- err
					return
				}
				if backoff == 0 {
					backoff = 30 * time.Second
				} else {
					backoff = min(backoff*2, 5*time.Minute)
				}
				delay = backoff
			} else {
				backoff = 0
			}
			if err := wait(ctx, delay); err != nil {
				if ctx.Err() != nil {
					err = nil
				}
				done <- err
				return
			}
		}
	}()
	return done, nil
}

// A raw capture can be durable even when Git verification/evidence recording
// exhausted its local budget. Recover that original artifact, never the process.
func (d *CLIDriver) recoverRawCLI(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	attempts, err := d.engine.PendingCLICaptureRecoveries(ctx, d.capability.ID, d.rawRecoveryCursor)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		d.rawRecoveryCursor = ""
		return nil
	}
	var failures []error
	for _, a := range attempts {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		d.rawRecoveryCursor = a.ID
		if err := d.recoverOneRawCLI(ctx, a); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (d *CLIDriver) recoverOneRawCLI(ctx context.Context, a reviewexec.Attempt) error {
	record, _, err := d.captures.Read(ctx, a.ID)
	if err != nil {
		return err
	}
	if record.Capture.Started && record.Capture.Exited {
		if err := d.engine.RecordCLIProcessExit(ctx, a.ProjectID, a.ID, record.FinishedAt); err != nil {
			return err
		}
	}
	anchor := *a.Anchor
	w, err := d.manager.Open(ctx, a.ID, anchor.CandidateSHA, anchor.BaseSHA, anchor.MergeBaseSHA, anchor.PatchSHA256, anchor.Repository)
	if err != nil {
		return err
	}
	_, err = RetainCLI(ctx, d.captures, d.engine, d.manager, a, w, record.Capture, record.StartedAt, record.FinishedAt)
	return err
}

// A restart ended the process of these attempts. Settle each one whose raw
// capture is either absent or already recorded as incomplete evidence; a
// capture still awaiting recording is left for raw recovery first.
func (d *CLIDriver) settleRestartedCLI(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	attempts, err := d.engine.RestartedCLIAttempts(ctx, d.capability.ID, d.restartCursor)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		d.restartCursor = ""
		return nil
	}
	d.restartCursor = attempts[len(attempts)-1].AttemptID
	var failures []error
	for _, a := range attempts {
		if !a.HasEvidence {
			exists, err := d.captures.Exists(a.AttemptID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if exists {
				continue
			}
		}
		if _, err := d.engine.SettleRestartedCLI(ctx, a); err != nil {
			failures = append(failures, err)
			continue
		}
		slog.InfoContext(ctx, "Settled a CLI review interrupted by a restart", "attempt_id", a.AttemptID)
	}
	return errors.Join(failures...)
}
