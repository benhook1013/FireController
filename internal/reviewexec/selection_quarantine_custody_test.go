package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestSelectionQuarantineCustodyFencesOrdinaryReaders(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) { checkSelectionQuarantineCustody(t, channel) })
	}
}

func checkSelectionQuarantineCustody(t *testing.T, channel string) {
	t.Helper()
	f := schedulingFixtureWithQueue(t, []int64{2, 4})
	if channel == "hosted" {
		schedulingEnable(t, f, 0, true)
		schedulingGenerate(t, f, true)
	} else {
		schedulingCLIEnable(t, f, 0, true)
		schedulingCLIGenerate(t, f, true)
	}
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	if r.State != "selected" || r.OperationID == "" {
		t.Fatal("missing ordinary synthetic child")
	}
	children := snapshotTable(t, f, "review_operations")
	actors := snapshotTable(t, f, "review_automatic_selections")
	events := snapshotTable(t, f, "events")
	// Exercise only the new canonical custody state, not corruption or an
	// attempt to dispatch. The held record cannot create positive authority.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_selection_quarantine(selection_id,project_id,original_operation_id,original_state,observed_revision)
		 VALUES(?,?,?,'selecting',?)`, r.ID, r.ProjectID, r.OperationID, r.Revision); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE review_selections SET state='held',reason='linked_selection_quarantined',revision=revision+1,
		  operation_id=NULL WHERE id=?`, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var op Operation
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		op, err = readOperation(ctx, tx, r.ProjectID, r.OperationID)
		if err != nil {
			return err
		}
		return f.e.requireAutomaticOperation(ctx, tx, op)
	})
	attemptCode(t, err, linkedSelectionCustodyHeld)
	if channel == "cli" {
		proceed, ready, explicit, err := f.e.checkCLIDraft(t.Context(), op, nil)
		attemptCode(t, err, linkedSelectionCustodyHeld)
		if proceed || ready || explicit {
			t.Fatal("retained child was reclassified as an explicit CLI op")
		}
	}
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, r.ProjectID)
		if err != nil {
			return err
		}
		limit, err := f.e.automaticHostedQueuedPrefixChannel(ctx, tx, r.ProjectID, c, channel)
		if err == nil && limit != len(c.Queue) {
			t.Fatal("proven held child still blocked the ordinary queue prefix", limit, c.Queue)
		}
		return err
	})
	if err != nil {
		t.Fatal("proven held child blocked the ordinary prefix", err)
	}
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		eligible, err := f.e.hostedPriorityBinding(ctx, tx, op)
		if err == nil && eligible {
			t.Fatal("proved held child occupied an ordinary queue turn")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.e.pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range pending {
		if candidate.ID == op.ID {
			t.Fatal("proved held child reentered operation worker scan")
		}
	}
	if children != snapshotTable(t, f, "review_operations") || actors != snapshotTable(t, f, "review_automatic_selections") || events != snapshotTable(t, f, "events") {
		t.Fatal("custody fence rewrote child, authorizer, or event history")
	}
	// A valid-looking settled pull which no longer agrees with the exact
	// retained child is unknown custody, not ordinary ineligibility.
	admissionSQL(t, f, `UPDATE review_automatic_selections SET pull_number=4 WHERE selection_id=?`, r.ID)
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return f.e.requireAutomaticOperation(ctx, tx, op)
	})
	attemptCode(t, err, "storage_unavailable")
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := f.e.hostedPriorityBinding(ctx, tx, op)
		return err
	})
	attemptCode(t, err, "storage_unavailable")
	admissionSQL(t, f, `UPDATE review_automatic_selections SET pull_number=? WHERE selection_id=?`, op.PullNumber, r.ID)

	// A malformed diagnostic record cannot certify ordinary ineligibility
	// in a read-only prefix; keep the existing conservative refusal.
	admissionSQL(t, f, `UPDATE review_selection_quarantine SET observed_revision=observed_revision+1 WHERE selection_id=?`, r.ID)
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, r.ProjectID)
		if err != nil {
			return err
		}
		_, err = f.e.automaticHostedQueuedPrefixChannel(ctx, tx, r.ProjectID, c, channel)
		return err
	})
	attemptCode(t, err, "storage_unavailable")

}

func TestSelectionQuarantineRefusesEqualNonTextActors(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	if r.State != "selected" || r.OperationID == "" {
		t.Fatal("missing retained child")
	}
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
			return err
		}
		for _, change := range []struct{ query, id string }{
			{`UPDATE review_operations SET actor_id=CAST(actor_id AS BLOB) WHERE id=?`, r.OperationID},
			{`UPDATE review_automatic_selections SET actor_id=CAST(actor_id AS BLOB) WHERE selection_id=?`, r.ID},
			{`UPDATE review_selections SET state='selecting',actor_id=CAST(actor_id AS BLOB) WHERE id=?`, r.ID},
		} {
			if _, err := tx.ExecContext(ctx, change.query, change.id); err != nil {
				_, resetErr := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`)
				return errors.Join(err, resetErr)
			}
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`); err != nil {
			return err
		}
		const selectionState = `SELECT json_array(quote(id),quote(project_id),quote(actor_id),quote(worker_id),quote(channel),quote(configuration_revision),quote(policy_revision),quote(capability_fingerprint),quote(expected_pull_number),quote(state),quote(reason),quote(revision),quote(operation_id),quote(created_at),quote(updated_at)) FROM review_selections WHERE project_id=? AND id=?`
		var before, after string
		if err := tx.QueryRowContext(ctx, selectionState, r.ProjectID, r.ID).Scan(&before); err != nil {
			return err
		}
		moved, err := quarantineNativeSelection(ctx, tx, r.ProjectID, r.ID, "selecting", r.Revision)
		if moved || !selectionCustodyUnproved(err) {
			t.Fatalf("malformed retained actors proved custody: moved=%t err=%v", moved, err)
		}
		if err := tx.QueryRowContext(ctx, selectionState, r.ProjectID, r.ID).Scan(&after); err != nil {
			return err
		}
		if before != after {
			t.Fatal("refused quarantine changed the original selection or child link")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var custody int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_selection_quarantine WHERE selection_id=?`, r.ID).Scan(&custody); err != nil || custody != 0 {
		t.Fatalf("unexpected custody record: count=%d err=%v", custody, err)
	}
}
