package reviews

import (
	"database/sql"
	"fmt"
	"testing"
)

func committedHistoryReferences(t *testing.T) *apiFixture {
	t.Helper()
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	for revision := int64(1); revision <= 3; revision++ {
		id := historyWriterSnapshot(t, tx, f.project, revision)
		write := newHistorySnapshotWrite(f.project, revision)
		if revision == 1 {
			historyWriterStage(t, tx, write, 7, 0, "superseded")
			historyWriterStage(t, tx, write, 8, 1, "defunct")
		} else if revision == 2 {
			historyWriterStage(t, tx, write, 7, 1, "defunct")
			historyWriterStage(t, tx, write, 8, 0, "defunct")
		}
		if err := write.applyCurrent(t.Context(), tx, id, revision-1); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal("valid retained graph rejected", err)
	}
	return f
}

func TestHistoryReferencesRejectLogicalDamageWithoutWriting(t *testing.T) {
	cases := []struct{ name, damage string }{
		{"missing current reference", `DELETE FROM review_current_retired WHERE project_id=? AND number=7`},
		{"closed current reference", `UPDATE review_current_retired SET start_revision=1,position=0 WHERE project_id=? AND number=7`},
		{"overlapping retained intervals", `UPDATE review_retired_memberships SET end_revision=3 WHERE project_id=? AND number=7 AND start_revision=1`},
		{"historical ordering collision", `UPDATE review_retired_memberships SET position=0 WHERE project_id=? AND number=8 AND start_revision=1`},
		{"mismatched workspace revision", `UPDATE review_workspace SET revision=2 WHERE project_id=?`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := committedHistoryReferences(t)
			tx := historyWriterTx(t, f)
			// The closed-current case must not collide with the other current order;
			// remove that unrelated reference so SQLite's unique index does not hide
			// the logical stale-reference fault the inspection boundary must reject.
			if tc.name == "closed current reference" {
				if _, err := tx.ExecContext(t.Context(), `DELETE FROM review_current_retired WHERE project_id=? AND number=8`, f.project); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), tc.damage, f.project); err != nil {
				t.Fatal(err)
			}
			var fkTable string
			var rowID sql.NullInt64
			var parent string
			var constraint int
			if err := tx.QueryRowContext(t.Context(), `PRAGMA foreign_key_check`).Scan(&fkTable, &rowID, &parent, &constraint); err != sql.ErrNoRows {
				t.Fatalf("fault should pass ordinary FK checks: %v", err)
			}
			var before, after int64
			if err := tx.QueryRowContext(t.Context(), `SELECT total_changes()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := ValidateHistoryReferences(t.Context(), tx); err == nil {
				t.Fatal("logical graph damage accepted")
			}
			if err := tx.QueryRowContext(t.Context(), `SELECT total_changes()`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("validation mutated the inspected database")
			}
		})
	}
}

func TestHistoryReferencesRequireOwningPullThroughoutRetainedVisibility(t *testing.T) {
	for _, kind := range []string{"run", "attempt"} {
		for _, damage := range []string{"missing", "historical-gap"} {
			t.Run(kind+"/"+damage, func(t *testing.T) {
				f := newAPIFixture(t)
				pull := nativePull(7)
				if kind == "run" {
					pull.Runs = []RunInput{nativeRun("parent-visibility")}
				} else {
					pull.Attempts = []Attempt{nativeAttempt("parent-visibility", "prepared")}
				}
				for revision := int64(0); revision < 3; revision++ {
					pull.Title = fmt.Sprintf("Owning PR version %d", revision)
					f.observe(t, reviewRequest(revision, []PullInput{pull}), fmt.Sprintf("parent-visibility-%d", revision))
				}
				if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
					t.Fatal("contiguous changed parent rejected", err)
				}
				tx := historyWriterTx(t, f)
				if damage == "missing" {
					if _, err := tx.Exec(`DELETE FROM review_current_pulls`); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.Exec(`DELETE FROM review_pull_memberships`); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := tx.Exec(`DELETE FROM review_pull_memberships WHERE start_revision=2`); err != nil {
						t.Fatal(err)
					}
				}
				var fkTable string
				var rowID sql.NullInt64
				var parent string
				var constraint int
				if err := tx.QueryRowContext(t.Context(), `PRAGMA foreign_key_check`).Scan(&fkTable, &rowID, &parent, &constraint); err != sql.ErrNoRows {
					t.Fatal("fixture must pass ordinary FK checks", err)
				}
				if err := ValidateHistoryReferences(t.Context(), tx); err == nil {
					t.Fatal("child visible without owning PR accepted")
				}
			})
		}
	}
}
