package reviewexec

import (
	"context"
	"errors"
	"testing"
)

func TestQueuedConfigurationFractionalRevisionIsLocalDamage(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	request := f.enqueue(t, attemptProject, "fractional-history", "cli", 2, 1)
	f.configure(t, attemptProject, "test-capability", 1, false)
	admissionSQL(t, f, `INSERT INTO review_execution_config(project_id,revision,body_json) SELECT project_id,1.5,body_json FROM review_execution_config WHERE project_id=? AND revision=1`, attemptProject)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	current, revision, err := readConfiguration(t.Context(), tx, attemptProject)
	if err != nil {
		t.Fatal(err)
	}
	o, err := readOperationBase(t.Context(), tx, attemptProject, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	compatible, err := compatibleQueuedConfiguration(t.Context(), tx, o, current, revision)
	if compatible || !isStoredUnavailable(err, storedConfigurationHistory) {
		t.Fatalf("fractional retained revision escaped candidate-local classification: compatible=%v err=%v", compatible, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations")
	if _, err := f.e.pending(t.Context()); err != nil {
		t.Fatal("damaged history stopped scheduler selection", err)
	}
	if before != snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "review_execution_attempts")+snapshotTable(t, f, "review_execution_reservations") {
		t.Fatal("damaged history changed operation identity or acquired capacity")
	}
}

func TestQueuedConfigurationHistoryPreservesOperationalErrors(t *testing.T) {
	for _, failure := range []string{"database", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if failure == "database" {
				if _, err := tx.ExecContext(t.Context(), `ALTER TABLE review_execution_config RENAME TO unavailable_config`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			compatible, err := compatibleQueuedConfiguration(ctx, tx, Operation{ProjectID: attemptProject, ConfigurationRevision: 1}, Configuration{}, 2)
			if compatible || err == nil || isStoredUnavailable(err, storedConfigurationHistory) {
				t.Fatal("operational error hidden as candidate damage", compatible, err)
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		})
	}
}
