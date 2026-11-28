package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestPreferredCLIClaimWithExpiredCooldown(t *testing.T) {
	for _, account := range []string{"test-capability", "other-account"} {
		t.Run(account, func(t *testing.T) {
			f, high, _, now := cliWaitFixture(t)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, high.ID, high.Revision); err != nil {
				t.Fatal(err)
			}
			*now = cliWaitDue(t, cliWaitRead(t, f, high.OperationID)).Add(time.Minute)
			admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, attemptProject)
			f.configure(t, otherAttemptProject, account, 0, false)
			low := f.enqueue(t, otherAttemptProject, "ordinary-claim", "cli", 2, 1)
			if account == "test-capability" {
				if a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID); a.ID != "" || err != nil {
					t.Fatalf("eligible preferred operation did not retain priority: %+v, %v", a, err)
				}
			}
			admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, high.OperationID)
			if a, err := f.e.StartAttempt(t.Context(), attemptProject, high.OperationID); a.ID != "" || !isWaitUnavailable(err) {
				t.Fatalf("preferred operation with missing wait was not refused: %+v, %v", a, err)
			}
			if a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID); a.State != "preparing" || err != nil {
				t.Fatalf("missing expired wait blocked a valid competing claim: %+v, %v", a, err)
			}
		})
	}
}

func TestPreferredCLIMalformedNativeWaitPause(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	low := f.enqueue(t, attemptProject, "ordinary-claim", "cli", 2, 1)
	high := f.enqueue(t, otherAttemptProject, "preferred-claim", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, otherAttemptProject)
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// Corruption fixture only: the normal CHECK prevents storing this TEXT.
		if _, err := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
			return err
		}
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO review_operation_waits(operation_id,paused) VALUES(?,'broken')`, high.ID)
		_, resetErr := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`)
		return errors.Join(insertErr, resetErr)
	}); err != nil {
		t.Fatal(err)
	}
	var storageClass string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT typeof(paused) FROM review_operation_waits WHERE operation_id=?`, high.ID).Scan(&storageClass); err != nil || storageClass != "text" {
		t.Fatalf("expected native SQLite TEXT pause, got %q, %v", storageClass, err)
	}
	if _, err := readOperationBase(t.Context(), f.store.DB(), otherAttemptProject, high.ID); !isWaitUnavailable(err) {
		t.Errorf("expected candidate-local malformed pause error, got %v", err)
	}
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID); a.State != "preparing" || err != nil {
		t.Fatalf("malformed preferred pause blocked a valid competing claim: %+v, %v", a, err)
	}
}

func TestPreferredCLIClaimSkipsWrongRoundBinding(t *testing.T) {
	f := roundFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, attemptProject)
	roundGrant(t, f, 2, "cli", 0, 0, 3, "preferred-round")
	roundGrant(t, f, 4, "cli", 0, 0, 3, "other-round")
	high := f.enqueue(t, attemptProject, "preferred", "cli", 2, 1)
	low := f.enqueue(t, otherAttemptProject, "ordinary", "cli", 2, 1)
	if a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID); err != nil || a.ID != "" {
		t.Fatalf("valid preferred request did not hold its turn: %+v %v", a, err)
	}
	admissionSQL(t, f, `UPDATE review_round_operation_bindings SET pull_number=4 WHERE operation_id=?`, high.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, high.ID); a.ID != "" || !isStoredUnavailable(err, storedRoundHistory) {
		t.Fatalf("wrong round binding was not refused on its own claim: %+v %v", a, err)
	}
	if a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID); err != nil || a.State != "preparing" {
		t.Fatalf("malformed preferred request blocked the healthy claim: %+v %v", a, err)
	}
}

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
