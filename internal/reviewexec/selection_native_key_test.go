package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestSelectionNativeBlobKeyDoesNotStopRecovery(t *testing.T) {
	for _, tc := range []struct{ state, key string }{
		{"queued", "id"},
		{"selecting", "id"},
		{"selecting", "project_id"},
	} {
		t.Run(tc.state+"/"+tc.key, func(t *testing.T) {
			f := selectionFixture(t)
			corrupt, _ := selectionEnqueue(t, f, "blob-selection", "hosted", nil)
			healthy, _ := selectionEnqueue(t, f, "healthy-selection", "cli", nil)
			// Queued scans order by age. Recovery orders by id, where SQLite
			// sorts text before blobs. Use a malformed project key as well so
			// a healthy selection can follow a malformed key in that scan.
			const corruptID = "review_selection_00000000000000000000000000000000"
			healthyID := healthy.ID
			admissionSQL(t, f, `UPDATE review_selections SET id=?,state=?,created_at=? WHERE id=?`, corruptID, tc.state, "2000-01-01T00:00:00Z", corrupt.ID)
			admissionSQL(t, f, `UPDATE review_selections SET state=?,created_at=? WHERE id=?`, tc.state, "2000-01-02T00:00:00Z", healthyID)
			var corruptKey any = []byte(corruptID)
			if tc.key == "id" {
				admissionSQL(t, f, `UPDATE review_selections SET id=CAST(id AS BLOB) WHERE id=?`, corruptID)
			} else {
				corruptKey = corruptID
				corruptSelectionProjectKey(t, f, corruptID)
			}

			if tc.state == "selecting" {
				if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := selectionRead(t, f, healthyID); got.State != "queued" || got.Revision != 2 {
					t.Fatalf("healthy selection was not recovered: %+v", got)
				}
			} else if worked, err := f.e.runNext(t.Context(), nil); err != nil || !worked {
				t.Fatal("malformed key did not complete an isolating worker pass", worked, err)
			}

			var state, reason string
			if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, corruptKey).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "held" || reason != "local_read_unavailable" {
				t.Fatal(state, reason)
			}
			selectionNext(t, f, &refreshTestReader{})
			if got := selectionRead(t, f, healthyID); got.State != "selected" || got.OperationID == "" {
				t.Fatalf("healthy selection did not progress: %+v", got)
			}
		})
	}
}

func corruptSelectionProjectKey(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); err != nil {
			t.Error(err)
		}
	}()
	if _, err := conn.ExecContext(t.Context(), `UPDATE review_selections SET project_id=CAST(project_id AS BLOB) WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedBlobSelectionWithChildDoesNotStopIndependentSelection(t *testing.T) {
	f := selectionFixture(t)
	broken, _ := selectionEnqueue(t, f, "linked-blob-selection", "hosted", nil)
	healthy, _ := selectionEnqueue(t, f, "healthy-linked-blob-neighbor", "cli", nil)
	child := f.enqueue(t, attemptProject, "retained-hosted-child", "hosted", 4, 1)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
			return err
		}
		_, corruptErr := tx.ExecContext(ctx, `UPDATE review_selections
			SET id=CAST(id AS BLOB),operation_id=?,created_at='2000-01-01T00:00:00Z' WHERE id=?`, child.ID, broken.ID)
		_, resetErr := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`)
		return errors.Join(corruptErr, resetErr)
	})
	if err != nil {
		t.Fatal(err)
	}
	selectionNext(t, f, &refreshTestReader{})
	if got := selectionRead(t, f, healthy.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatalf("independent selection did not progress: %+v", got)
	}
	var state, linkedID string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT state,operation_id FROM review_selections WHERE id=?`, []byte(broken.ID)).Scan(&state, &linkedID); err != nil || state != "queued" || linkedID != child.ID {
		t.Fatalf("unproved custody was rewritten: %q %q %v", state, linkedID, err)
	}
}
