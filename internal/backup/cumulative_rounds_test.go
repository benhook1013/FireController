package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Preserve the exact pre-default enrollment transaction in recovery fixtures.
// This records a genuine predecessor configuration, receipt and event; it does
// not disable or replace the current Configure path used for later enrollments.
func seedPredecessorExecutionConfiguration(t *testing.T, f *restoreFixture, scope core.Scope, key string, configuration reviewexec.Configuration) {
	t.Helper()
	_, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "reviews.configure", Target: "configuration", Key: key, Request: configuration}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var previous int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM review_execution_config WHERE project_id=?`, f.project).Scan(&previous); err != nil {
			return nil, err
		}
		if configuration.ExpectedRevision == nil || *configuration.ExpectedRevision != previous {
			return nil, core.Fail("fixture_revision_mismatch", "Predecessor configuration revision mismatch.", 409)
		}
		body, err := json.Marshal(configuration)
		if err != nil {
			return nil, err
		}
		next := previous + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, f.project, next, string(body)); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.configured", f.project, next, map[string]any{"revision": next, "paused": configuration.Paused}); err != nil {
			return nil, err
		}
		return reviewexec.Receipt{ID: f.project, State: "configured", Revision: next}, nil
	})
	if err != nil {
		t.Fatal("seed authentic predecessor configuration", err)
	}
}

func TestCumulativeRoundInventoryV4AndLegacyV3PreserveHistoryAndCompletions(t *testing.T) {
	for _, version := range []int{4, 3} {
		t.Run(map[int]string{4: "cumulative-v4", 3: "legacy-v3"}[version], func(t *testing.T) {
			f := roundInventoryFixture(t, false)
			if version == 4 {
				e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
				if err != nil {
					t.Fatal(err)
				}
				previous, minimum, maximum := int64(2), int64(0), int64(1000)
				if _, err := e.ConfigureRoundControl(t.Context(), routePolicyInventoryScope(t, f), "cumulative-extra-thousand", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &previous, Action: "grant", Minimum: &minimum, Maximum: &maximum}); err != nil {
					t.Fatal(err)
				}
				control, err := e.ReadRoundControl(t.Context(), routePolicyInventoryScope(t, f), 4, "cli")
				if err != nil || control.Epoch != 1 || control.Used != 1 || control.Dry != 1 || control.Maximum != 1001 {
					t.Fatal("fixture lost cumulative thresholds or completion credit", control, err)
				}
			} else {
				// A first grant, one completion and a close have identical legacy
				// accounting. Retain their genuine native graph while restoring
				// the exact v3 declaration and unwrapped legacy history format.
				rows, err := f.store.DB().Query(`SELECT revision,body_json FROM review_round_changes ORDER BY revision`)
				if err != nil {
					t.Fatal(err)
				}
				type legacyChange struct {
					revision int64
					body     string
				}
				var changes []legacyChange
				for rows.Next() {
					var revision int64
					var body string
					if err := rows.Scan(&revision, &body); err != nil {
						t.Fatal(err)
					}
					var history struct {
						Format  int             `json:"format"`
						Control json.RawMessage `json:"control"`
					}
					if err := json.Unmarshal([]byte(body), &history); err != nil || history.Format != 2 || len(history.Control) == 0 {
						t.Fatal("expected genuine format2 fixture history", err)
					}
					changes = append(changes, legacyChange{revision, string(history.Control)})
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
				for _, change := range changes {
					if _, err := f.store.DB().Exec(`UPDATE review_round_changes SET body_json=? WHERE revision=?`, change.body, change.revision); err != nil {
						t.Fatal(err)
					}
				}
				for _, statement := range []string{
					`ALTER TABLE review_round_controls RENAME TO cumulative_fixture_controls`, reviewexec.RoundSchemaV3SQL()[0],
					`INSERT INTO review_round_controls SELECT * FROM cumulative_fixture_controls`, `DROP TABLE cumulative_fixture_controls`,
					`UPDATE metadata SET value='3' WHERE key='schema:review_rounds'`,
				} {
					if _, err := f.store.DB().Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatal("source round snapshot invalid", err)
			}
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if checked, err := Inspect(t.Context(), bundle.Path); err != nil || checked.State != "verified" {
				t.Fatal("supported round version refused by inspect", checked, err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-rounds")
			if restored, err := Restore(t.Context(), bundle.Path, destination); err != nil || restored.State != "inactive" {
				t.Fatal("supported round version refused by restore", restored, err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
				t.Fatal("restored round history/completions invalid", err)
			}
			var restoredVersion, completions int
			if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&restoredVersion); err != nil || restoredVersion != version {
				t.Fatal("inactive restore changed round schema version", restoredVersion, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM review_round_completions`).Scan(&completions); err != nil || completions != 1 {
				t.Fatal("native completion missing after recovery", completions, err)
			}
			after := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && after[table] != rows {
					t.Fatal("restore changed retained table", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("inspect/restore modified source or immutable bundle")
			}
		})
	}
}
