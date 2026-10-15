package backup

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestJobProgressBackupRoundTripPreservesIndependentHistory(t *testing.T) {
	f := newRestoreFixture(t)
	jobs := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)
	jobID := jobs[0][0].(string)
	base := "/api/v1/projects/" + f.project + "/jobs/" + jobID
	f.request(t, "POST", base+"/updates", map[string]any{"expected_job_revision": 1, "body": "Independent retained progress <literal> 雪", "evidence": []string{"ref-first", "ref-second"}})
	f.request(t, "PUT", base+"/checklist", map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": []map[string]any{{"id": "stable-one", "text": "Initial plan", "done": false}, {"id": "stable-two", "text": "Next plan", "done": true}}})
	f.request(t, "PUT", base+"/checklist", map[string]any{"expected_job_revision": 1, "expected_revision": 1, "items": []map[string]any{{"id": "stable-two", "text": "Next plan", "done": false}}})
	tables := []string{"jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "events", "receipts"}
	original := map[string][][]any{}
	for _, name := range tables {
		original[name] = restoreRows(t, f.store.DB(), `SELECT * FROM `+name+` ORDER BY rowid`)
	}
	bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("jobs3 inspection failed", err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal("jobs3 restore failed", err)
	}
	for _, name := range tables {
		if !reflect.DeepEqual(original[name], generationRead(t, destination, `SELECT * FROM `+name+` ORDER BY rowid`)) {
			t.Fatal("independent progress or receipt history changed", name)
		}
		if !reflect.DeepEqual(original[name], restoreRows(t, f.store.DB(), `SELECT * FROM `+name+` ORDER BY rowid`)) {
			t.Fatal("backup changed source", name)
		}
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection/restore modified immutable bundle")
	}
}
