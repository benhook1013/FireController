package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

// An earlier PR whose own unresolved attempt prevents it from starting must
// not hold back a later PR's review.
func TestEarlierBlockedPRDoesNotHoldLaterPR(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	earlier := f.enqueue(t, attemptProject, "earlier-blocked", "cli", 2, 1)
	later := f.enqueue(t, attemptProject, "later-independent", "cli", 4, 1)
	check := func() *Operation {
		t.Helper()
		var found *Operation
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			o, err := readOperation(ctx, tx, attemptProject, later.ID)
			if err != nil {
				return err
			}
			c, revision, err := readConfiguration(ctx, tx, attemptProject)
			if err != nil {
				return err
			}
			found, err = f.e.earlierQueuedReviewIntent(ctx, tx, o, c, revision)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	if found := check(); found == nil || found.ID != earlier.ID {
		t.Fatal("startable earlier PR lost priority", found)
	}
	a := f.start(t, attemptProject, earlier)
	// Model a fresh request on a PR whose earlier attempt is still unresolved.
	if _, err := f.store.DB().Exec(`UPDATE review_operations SET state='queued',attempt_id='' WHERE id=?`, a.OperationID); err != nil {
		t.Fatal(err)
	}
	if found := check(); found != nil {
		t.Fatal("blocked earlier PR still holds the later PR", found.ID)
	}
}
