package backup

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPrimaryFocusBackupRoundTripAndStrictSchema(t *testing.T) {
	f := newRestoreFixture(t)
	job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	path := "/api/v1/projects/" + f.project + "/workers/" + f.worker + "/primary-job"
	for revision, selected := range []string{job, "", job} {
		f.request(t, "PUT", path, map[string]any{"expected_revision": revision, "job_id": selected})
	}
	tables := []string{"worker_primary_jobs", "worker_primary_job_history", "jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "receipts", "events"}
	original := map[string][][]any{}
	for _, table := range tables {
		original[table] = restoreRows(t, f.store.DB(), `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("jobs4 inspect", err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	summary, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil {
		t.Fatal("jobs4 restore", err)
	}
	if summary.InstanceID == f.store.InstanceID() || summary.State != "inactive" {
		t.Fatal(summary)
	}
	for _, table := range tables {
		if !reflect.DeepEqual(original[table], generationRead(t, destination, `SELECT * FROM `+table+` ORDER BY rowid`)) {
			t.Fatal("restore changed focus/standing ledger", table)
		}
		if !reflect.DeepEqual(original[table], restoreRows(t, f.store.DB(), `SELECT * FROM `+table+` ORDER BY rowid`)) {
			t.Fatal("source modified", table)
		}
	}
	if got := generationRead(t, destination, `SELECT count(*) FROM credentials WHERE revoked=0`)[0][0].(int64); got != 0 {
		t.Fatal("restored credentials revived", got)
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("Inspect/Restore changed immutable bundle")
	}
	for _, change := range []string{
		`ALTER TABLE worker_primary_jobs ADD COLUMN unexpected TEXT`,
		`DROP TABLE worker_primary_job_history`,
		`CREATE TABLE altered_primary(project_id TEXT NOT NULL,worker_id TEXT NOT NULL,revision INTEGER NOT NULL,job_id TEXT,PRIMARY KEY(project_id,worker_id),FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id) ON DELETE CASCADE,FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id));INSERT INTO altered_primary SELECT * FROM worker_primary_jobs;DROP TABLE worker_primary_jobs;ALTER TABLE altered_primary RENAME TO worker_primary_jobs`,
	} {
		t.Run(fmt.Sprintf("schema-%d", len(change)), func(t *testing.T) {
			fresh, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, fresh.Path, change)
			before := restoreTree(t, fresh.Path)
			_, err = Inspect(t.Context(), fresh.Path)
			backupCode(t, err, "backup_invalid")
			_, err = Restore(t.Context(), fresh.Path, filepath.Join(t.TempDir(), "refused"))
			backupCode(t, err, "backup_invalid")
			if !reflect.DeepEqual(before, restoreTree(t, fresh.Path)) {
				t.Fatal("invalid inventory mutated")
			}
		})
	}
}

func TestPrimaryFocusOlderJobSchemaBundlesRemainReadOnly(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newRestoreFixture(t)
			statements := `DROP TABLE worker_primary_job_history;DROP TABLE worker_primary_jobs;`
			if version < 3 {
				statements += `DROP TABLE job_updates;DROP TABLE job_checklists;DROP TABLE job_checklist_history;`
			}
			if version < 2 {
				statements += `DROP TABLE job_checkpoints;`
			}
			statements += fmt.Sprintf(`UPDATE metadata SET value='%d' WHERE key='schema:jobs'`, version)
			if _, err := f.store.DB().Exec(statements); err != nil {
				t.Fatal(err)
			}
			original := restoreRows(t, f.store.DB(), `SELECT * FROM metadata ORDER BY key`)
			bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal("older create", err)
			}
			before := restoreTree(t, bundle.Path)
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("older inspect", err)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal("older restore", err)
			}
			got := generationRead(t, destination, `SELECT value FROM metadata WHERE key='schema:jobs'`)[0][0].(string)
			if got != fmt.Sprint(version) {
				t.Fatal("restore migrated old jobs", got)
			}
			if rows := generationRead(t, destination, `SELECT name FROM sqlite_schema WHERE type='table' AND name LIKE 'worker_primary_%'`); len(rows) != 0 {
				t.Fatal("old restore synthesized focus")
			}
			if !reflect.DeepEqual(original, restoreRows(t, f.store.DB(), `SELECT * FROM metadata ORDER BY key`)) || !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("older source/bundle modified")
			}
		})
	}
}
