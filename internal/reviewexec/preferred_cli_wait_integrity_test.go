package reviewexec

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOperationWaitPreservesOperationalErrors(t *testing.T) {
	f := schedulingFixture(t)
	r := f.enqueue(t, attemptProject, "wait-read", "cli", 2, 1)
	o, err := readOperationIdentity(t.Context(), f.store.DB(), attemptProject, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readOperationWait(ctx, f.store.DB(), o); !errors.Is(err, context.Canceled) || isWaitUnavailable(err) {
		t.Fatalf("cancellation lost its classification: %v", err)
	}
	admissionSQL(t, f, `ALTER TABLE review_operation_waits RENAME TO test_unavailable_waits`)
	if _, err := readOperationWait(t.Context(), f.store.DB(), o); err == nil || isWaitUnavailable(err) {
		t.Fatalf("missing table was not an operational failure: %v", err)
	}
}

func TestCLICooldownChecksDetachedPreparationJournal(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", cliDraftWait} {
		t.Run(reason, func(t *testing.T) {
			f, baseline, _, now := cliWaitFixture(t)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, baseline.ID, baseline.Revision); err != nil {
				t.Fatal(err)
			}
			wait := cliWaitRead(t, f, baseline.OperationID).Wait
			*now = mustWaitTime(wait.RetryNotBefore).Add(time.Minute)
			retry := f.start(t, attemptProject, Receipt{ID: baseline.OperationID})
			if _, err := f.e.Advance(t.Context(), attemptProject, retry.ID, retry.Revision, "held", reason, ""); err != nil {
				t.Fatal(err)
			}
			if got := cliWaitRead(t, f, baseline.OperationID); got.Wait == nil || got.Wait.BaselineAttemptID != baseline.ID || got.Wait.RetryNotBefore != wait.RetryNotBefore {
				t.Fatalf("unused retry lost the original cooldown: %+v", got.Wait)
			}
			// Preserve the detached attempt and its final hold, but invalidate the
			// exact first transition that should prove it was only a preparation.
			admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='not_preparing' WHERE attempt_id=? AND revision=1`, retry.ID)
			if _, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, baseline.OperationID); !isWaitUnavailable(err) {
				t.Fatalf("accepted detached hold with an invalid earlier transition: %v", err)
			}
		})
	}
}
