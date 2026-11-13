package backup

import (
	"path/filepath"
	"reflect"
	"testing"
)

func seedUnifiedJobProgress(t *testing.T, f *restoreFixture) string {
	t.Helper()
	id := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	path := "/api/v1/projects/" + f.project + "/jobs/" + id
	f.request(t, "PATCH", path, map[string]any{"expected_revision": 1, "status": "blocked", "progress": map[string]any{"summary": "Verified current work <literal> 雪", "next": "Wait for the selected dependency", "blocker": "Dependency remains open", "evidence": []string{"commit:synthetic", "test:focused"}}})
	f.request(t, "PATCH", path, map[string]any{"expected_revision": 2, "progress": map[string]any{"summary": "Dependency resolved", "next": "Run the actual workload", "blocker": "", "evidence": []string{}}})
	return id
}

func TestUnifiedJobProgressBackupPreservesCurrentAndHistory(t *testing.T) {
	f := newRestoreFixture(t)
	seedUnifiedJobProgress(t, f)
	tables := []string{"jobs", "job_history", "job_progress", "job_progress_history", "job_checkpoints", "job_updates", "receipts", "events"}
	before := map[string][][]any{}
	for _, table := range tables {
		before[table] = restoreRows(t, f.store.DB(), `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		query := `SELECT * FROM ` + table + ` ORDER BY rowid`
		if !reflect.DeepEqual(before[table], generationRead(t, destination, query)) {
			t.Fatal("inactive restore changed job records", table)
		}
		if !reflect.DeepEqual(before[table], restoreRows(t, f.store.DB(), query)) {
			t.Fatal("inspection changed source", table)
		}
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection or restore changed bundle")
	}
}

func TestUnifiedJobProgressBackupRefusesResealedCorruption(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"missing-current", `DELETE FROM job_progress`},
		{"mismatched-current", `UPDATE job_progress SET summary='Not the retained latest state'`},
		{"future-revision", `UPDATE job_progress SET job_revision=999`},
		{"null-evidence", `UPDATE job_progress_history SET evidence_json='null'`},
		{"cross-job-current", `PRAGMA foreign_keys=OFF;UPDATE job_progress SET job_id='job_missing'`},
		{"missing-history-index", `DROP INDEX job_progress_history_project_job_sequence`},
		{"wrong-history-index", `DROP INDEX job_progress_history_project_job_sequence;CREATE INDEX job_progress_history_project_job_sequence ON job_progress_history(project_id,job_id,sequence ASC)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			seedUnifiedJobProgress(t, f)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, test.sql)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err == nil {
				t.Fatal("resealed job corruption accepted")
			}
			if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("failed inspection changed bundle")
			}
		})
	}
}

func TestUnifiedJobProgressPredecessorBackupStaysUnmodified(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := f.store.DB().Exec(`DROP TABLE job_progress_history;DROP TABLE job_progress;UPDATE metadata SET value='7' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("supported schema-seven bundle refused", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	if got := generationRead(t, destination, `SELECT value FROM metadata WHERE key='schema:jobs'`)[0][0]; got != "7" {
		t.Fatal("inactive inspection migrated historical jobs", got)
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("historical bundle modified")
	}
}
