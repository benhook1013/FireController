package backup

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Reconstruct the exact populated predecessor CHECK before exercising the real
// migration. Native evidence and pending work stay intact.
func roundTaperPredecessor(t *testing.T, f *restoreFixture) {
	t.Helper()
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TEMP TABLE taper_predecessor_changes AS SELECT * FROM review_round_changes`,
		`DROP TABLE review_round_changes`,
		reviewexec.RoundSchemaV4SQL()[1],
		`INSERT INTO review_round_changes SELECT * FROM taper_predecessor_changes`,
		`DROP TABLE taper_predecessor_changes`,
		`UPDATE metadata SET value='4' WHERE key='schema:review_rounds'`,
	} {
		if _, err = conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatal("reconstruct populated version four", err)
		}
	}
	if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
}

func lowerRoundTaper(t *testing.T, f *restoreFixture) {
	t.Helper()
	before := backupRows(t, f.store.DB())
	if err := reviewexec.EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal("upgrade populated round schema", err)
	}
	after := backupRows(t, f.store.DB())
	for table, rows := range before {
		if table != "metadata" && after[table] != rows {
			t.Fatal("schema upgrade changed retained rows", table)
		}
	}
	e, err := reviewexec.New(f.store, []reviewexec.Capability{
		{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)},
		{ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	prior, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil || prior.Taper != 2 || prior.InitialDry != 0 || prior.Used != 1 || prior.Dry != 1 || !prior.Closed {
		t.Fatal("predecessor native state missing", prior, err)
	}
	var request reviewexec.RoundControlRequest
	if err = json.Unmarshal([]byte(`{"expected_revision":2,"action":"taper","taper":1}`), &request); err != nil {
		t.Fatal(err)
	}
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "lower-taper-without-reopening", 4, "cli", request); err != nil {
		t.Fatal(err)
	}
	current, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil || current.Taper != 1 || current.Revision != prior.Revision+1 || !current.Closed || !current.Tapered {
		t.Fatal("taper edit did not preserve closed completed state", current, err)
	}
	current.Taper, current.Revision, current.UpdatedAt, current.Tapered = prior.Taper, prior.Revision, prior.UpdatedAt, prior.Tapered
	if !reflect.DeepEqual(current, prior) {
		t.Fatal("taper edit changed grant, counters, epoch or imported provenance", prior, current)
	}
	var grantRevision int64
	if err = f.store.DB().QueryRow(`SELECT grant_revision FROM review_round_controls WHERE project_id=? AND pull_number=4 AND channel='cli'`, f.project).Scan(&grantRevision); err != nil || grantRevision != 1 {
		t.Fatal("taper edit replaced the original grant", grantRevision, err)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("new taper history invalid", err)
	}
}

func TestRoundTaperInventoryPopulatedUpgradeAndInactiveRestore(t *testing.T) {
	f := roundInventoryFixture(t, false)
	roundTaperPredecessor(t, f)
	// Inspection and inactive restore accept the original version-four bundle
	// without upgrading its schema or rewriting its policy history.
	for _, version := range []int{4, 5} {
		if version == 5 {
			lowerRoundTaper(t, f)
		}
		before := backupRows(t, f.store.DB())
		bundle := restoreBundle(t, f)
		immutable := restoreTree(t, bundle.Path)
		if checked, err := Inspect(t.Context(), bundle.Path); err != nil || checked.State != "verified" {
			t.Fatal("supported taper schema refused", version, checked, err)
		}
		destination := filepath.Join(t.TempDir(), "inactive-taper")
		if restored, err := Restore(t.Context(), bundle.Path, destination); err != nil || restored.State != "inactive" {
			t.Fatal("inactive taper restore failed", version, restored, err)
		}
		db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
			db.Close()
			t.Fatal(err)
		}
		var restoredVersion, pending, completions, changes int
		if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&restoredVersion); err != nil || restoredVersion != version {
			t.Fatal("inactive restore altered schema version", restoredVersion, err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM review_execution_attempts WHERE state='preparing'`).Scan(&pending); err != nil || pending == 0 {
			t.Fatal("pending review disappeared", pending, err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM review_round_completions`).Scan(&completions); err != nil || completions != 1 {
			t.Fatal("native completion changed", completions, err)
		}
		expectedChanges := 2
		if version == 5 {
			expectedChanges++
		}
		if err = db.QueryRow(`SELECT count(*) FROM review_round_changes WHERE project_id=? AND pull_number=4 AND channel='cli'`, f.project).Scan(&changes); err != nil || changes != expectedChanges {
			t.Fatal("taper policy history changed", changes, err)
		}
		after := backupRows(t, db)
		db.Close()
		for table, rows := range before {
			if table != "metadata" && table != "credentials" && after[table] != rows {
				t.Fatal("restore altered retained work or taper authority", table)
			}
		}
		if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
			t.Fatal("inspect/restore changed source or bundle")
		}
	}
}

func TestRoundTaperInventoryResealedStateAndHistoryRefused(t *testing.T) {
	for name, query := range map[string]string{
		"changed-current-taper": `UPDATE review_round_controls SET taper=3 WHERE pull_number=4 AND channel='cli'`,
		"changed-taper-history": `UPDATE review_round_changes SET body_json=json_set(body_json,'$.control.taper',3) WHERE action='taper'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := roundInventoryFixture(t, false)
			roundTaperPredecessor(t, f)
			lowerRoundTaper(t, f)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("refused taper bundle changed source")
			}
		})
	}
}
