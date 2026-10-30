package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestSourceRefusalBackupPreservesOriginalComparison(t *testing.T) {
	for _, variant := range []struct {
		reason string
		value  reviewexec.SourceRefusal
	}{
		{"predecessor_parent_moved", reviewexec.SourceRefusal{PullNumber: 4, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}},
		{"parent_unavailable", reviewexec.SourceRefusal{PullNumber: 2, BaseRef: "codex/former-parent"}},
		{"predecessor_unfinished", reviewexec.SourceRefusal{PullNumber: 4, Cause: "discovery_incomplete"}},
	} {
		t.Run(variant.reason, func(t *testing.T) { sourceRefusalBackup(t, variant.reason, variant.value) })
	}
}
func sourceRefusalBackup(t *testing.T, reason string, value reviewexec.SourceRefusal) {
	t.Helper()
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := "3"
	if reason == "predecessor_parent_moved" {
		wantVersion = "1"
		if _, err := f.store.DB().Exec(`UPDATE metadata SET value='1' WHERE key='schema:review_source_refusals'`); err != nil {
			t.Fatal(err)
		}
	}
	var attempt string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_attempts WHERE state='preparing'`).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Advance(t.Context(), f.project, attempt, 1, "held", reason, ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET source_refusal_json=? WHERE attempt_id=? AND state='held'`, string(body), attempt); err != nil {
		t.Fatal(err)
	}
	if reason == "parent_unavailable" || reason == "predecessor_unfinished" {
		oldVersion := "1"
		wantVersion = "2"
		if reason == "predecessor_unfinished" {
			oldVersion, wantVersion = "2", "3"
		}
		if _, err := f.store.DB().Exec(`UPDATE metadata SET value=? WHERE key='schema:review_source_refusals'`, oldVersion); err != nil {
			t.Fatal(err)
		}
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		refused := reviewexec.ValidateSourceRefusalsSnapshot(t.Context(), tx)
		tx.Rollback()
		if refused == nil {
			t.Fatal("new refusal accepted under old format metadata")
		}
		if _, err := f.store.DB().Exec(`UPDATE metadata SET value=? WHERE key='schema:review_source_refusals'`, wantVersion); err != nil {
			t.Fatal(err)
		}
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("source-refusal snapshot inspection", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil || version != wantVersion {
		t.Fatal("format gate was lost", version, err)
	}
	var restored string
	if err := db.QueryRow(`SELECT source_refusal_json FROM review_execution_transitions WHERE attempt_id=? AND state='held'`, attempt).Scan(&restored); err != nil || restored != string(body) {
		t.Fatal("backup changed original refusal facts", restored, err)
	}
}
