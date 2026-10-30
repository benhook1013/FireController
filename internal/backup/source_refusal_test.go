package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestSourceRefusalBackupPreservesOriginalComparison(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	var attempt string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_attempts WHERE state='preparing'`).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Advance(t.Context(), f.project, attempt, 1, "held", "predecessor_parent_moved", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(reviewexec.SourceRefusal{PullNumber: 4, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)})
	if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET source_refusal_json=? WHERE attempt_id=? AND state='held'`, string(body), attempt); err != nil {
		t.Fatal(err)
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
	var restored string
	if err := db.QueryRow(`SELECT source_refusal_json FROM review_execution_transitions WHERE attempt_id=? AND state='held'`, attempt).Scan(&restored); err != nil || restored != string(body) {
		t.Fatal("backup changed original refusal facts", restored, err)
	}
}
