package reviewexec

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestInitialAutomaticSourceAcceptanceRequiresCanonicalEvidence(t *testing.T) {
	f := selectionFixture(t)
	accepted, _ := selectionEnqueue(t, f, "acceptance-contract-fixture", "hosted", nil)
	selection := selectionRead(t, f, accepted.ID)
	var missing Run = selection
	missing.ID = "no-retained-acceptance"
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		check := func(r Run, expected bool, expectError bool) {
			t.Helper()
			got, err := verifiedInitialAutomaticSourceAcceptance(ctx, tx, r)
			if got != expected || (err != nil) != expectError {
				t.Fatalf("canonical automatic acceptance = %v (%v), want %v (error=%v)", got, err, expected, expectError)
			}
		}
		check(missing, false, false)
		check(selection, false, false) // An explicit receipt is not an automatic grant.
		automatic := encoded(selectionRequestedEvent{Channel: selection.Channel, Origin: selectionOriginAutomatic, WorkerID: selection.WorkerID})
		if _, err := tx.ExecContext(ctx, `UPDATE events SET payload_json=? WHERE project_id=? AND resource_id=? AND event_type='review.selection_requested' AND revision=1`, automatic, selection.ProjectID, selection.ID); err != nil {
			return err
		}
		check(selection, true, false)
		other := selection
		other.WorkerID = "different-test-worker"
		check(other, false, true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A valid restart's retained revision order follows journal append order.
func TestInitialAutomaticSourceAcceptsOrderedRestartJournal(t *testing.T) {
	f := selectionFixture(t)
	accepted, _ := selectionEnqueue(t, f, "restart-journal-order", "hosted", nil)
	selection := selectionRead(t, f, accepted.ID)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, state := range []string{"selecting", "queued", "selecting"} {
			run, err := readRun(ctx, tx, selection.ProjectID, accepted.ID)
			if err != nil {
				return err
			}
			if err := updateRunAt(ctx, tx, run, state, "", "", time.Now()); err != nil {
				return err
			}
		}
		run, err := readRun(ctx, tx, selection.ProjectID, accepted.ID)
		if err != nil {
			return err
		}
		valid, err := automaticSourceInitialRead(ctx, tx, run)
		if !valid || err != nil {
			t.Fatalf("classify canonical restart journal: got %v (%v), want true", valid, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
