package reviewexec

import (
	"context"
	"database/sql"
	"testing"
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
