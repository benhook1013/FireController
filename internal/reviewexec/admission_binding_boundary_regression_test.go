package reviewexec

import (
	"context"
	"testing"
)

func TestAdmitReleasesOnlyMalformedSelectionLink(t *testing.T) {
	for _, outcome := range []string{"malformed-link", "proved-custody"} {
		t.Run(outcome, func(t *testing.T) {
			f := selectionFixtureWithQueue(t, []int64{2})
			r, _ := selectionEnqueue(t, f, "prepare-boundary", "hosted", nil)
			selectionNext(t, f, &refreshTestReader{})
			id := selectionRead(t, f, r.ID).OperationID
			if id == "" {
				t.Fatal("fixture did not retain a selected child")
			}
			before := roundOperation(t, f, id)
			var prep Attempt
			driver := &controlledDriver{prepare: func(ctx context.Context, op Operation, _ Configuration, a Attempt) (Preparation, error) {
				if op.ID != id {
					t.Fatal("unrelated operation entered fixture", op.ID)
				}
				if _, err := f.e.ExecutionContext(ctx, op.ProjectID, op.ID); err != nil {
					t.Fatal("expected valid preflight before controlled state change", err)
				}
				prep = a
				if outcome == "malformed-link" {
					admissionSQL(t, f, "UPDATE review_selections SET worker_id=? WHERE id=?", "different-fixture-worker", r.ID)
				} else {
					admissionSQL(t, f, "UPDATE review_selections SET state='held',reason='linked_selection_quarantined',operation_id=NULL,revision=3 WHERE id=?", r.ID)
					admissionSQL(t, f, "INSERT INTO review_selection_quarantine (selection_id,project_id,original_operation_id,original_state,observed_revision) VALUES(?,?,?,'selecting',2)", r.ID, attemptProject, id)
					if err := requireUnquarantinedSelectionLink(ctx, f.store.DB(), attemptProject, id); !isLinkedSelectionCustodyHeld(err) {
						t.Fatal("fixture has no proved custody", err)
					}
				}
				return Preparation{Anchor: attemptAnchor(op.ProjectID, op.PullNumber), Intent: "unposted-boundary-intent"}, nil
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal("local refusal became a worker-wide error", err)
			}
			after := roundOperation(t, f, id)
			if prep.ID == "" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 ||
				after.ID != before.ID || after.HeadSHA != before.HeadSHA || after.ActorID != before.ActorID ||
				after.WorkerID != before.WorkerID || after.ConfigurationRevision != before.ConfigurationRevision {
				t.Fatal("preparation changed original operation or dispatched", before, after)
			}
			expectedAttempt := "preparing"
			expectedReservations := 1
			if outcome == "malformed-link" {
				expectedAttempt, expectedReservations = "held", 0
				if after.State != "queued" || after.Reason != "automatic_scheduling_unavailable" || after.AttemptID != "" {
					t.Fatal("malformed direct link did not release unused preparation", after)
				}
			} else if after.State != "preparing" || after.AttemptID != prep.ID {
				t.Fatal("proved custody was treated as malformed direct link", after)
			}
			if n := attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations"); n != expectedReservations {
				t.Fatal("unexpected reservation count", n, expectedReservations)
			}
			var state, intent, external string
			if err := f.store.DB().QueryRow("SELECT state,intent,external_id FROM review_execution_attempts WHERE id=?", prep.ID).Scan(&state, &intent, &external); err != nil || state != expectedAttempt || intent != "" || external != "" {
				t.Fatal("refusal recorded external state or unexpected attempt lifecycle", state, intent, external, err)
			}
		})
	}
}
