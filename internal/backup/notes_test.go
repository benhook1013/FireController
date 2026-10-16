package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func seedBackupNotes(t *testing.T, f *restoreFixture) string {
	t.Helper()
	base := "/api/v1/projects/" + f.project + "/notes"
	job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	id := f.request(t, "POST", base, map[string]any{"worker_id": f.worker, "job_id": job, "phase": "review", "kind": "source", "body": "Private <literal> 雪\n\t source"})["id"].(string)
	f.request(t, "PATCH", base+"/"+id, map[string]any{"expected_revision": 1, "status": "dismissed", "status_reason": "Audited <reason>"})
	f.request(t, "PATCH", base+"/"+id, map[string]any{"expected_revision": 2, "body": strings.Repeat("<", 16<<10)})
	f.request(t, "PATCH", base+"/"+id, map[string]any{"expected_revision": 3, "status": "pending"})
	return id
}

func TestNotesBackupRoundTripPreservesCurrentAndEveryRevision(t *testing.T) {
	f := newRestoreFixture(t)
	seedBackupNotes(t, f)
	tables := []string{"notes", "note_revisions", "jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "worker_primary_jobs", "worker_primary_job_history", "worker_pause_state", "worker_pause_history", "inbox_messages", "receipts", "events"}
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
		t.Fatal("jobs6 inspect", err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
		t.Fatal(restored, err)
	}
	for _, table := range tables {
		if !reflect.DeepEqual(original[table], generationRead(t, destination, `SELECT * FROM `+table+` ORDER BY rowid`)) || !reflect.DeepEqual(original[table], restoreRows(t, f.store.DB(), `SELECT * FROM `+table+` ORDER BY rowid`)) {
			t.Fatal("notes/history/unrelated source changed", table)
		}
	}
	if generationRead(t, destination, `SELECT count(*) FROM credentials WHERE revoked=0`)[0][0].(int64) != 0 || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("restore revived access or edited bundle")
	}
}

func TestNotesBackupOlderJobsOneThroughFiveRemainUnmigrated(t *testing.T) {
	for version := 1; version <= 5; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newRestoreFixture(t)
			query := `DROP TABLE note_revisions;DROP TABLE notes;`
			if version < 5 {
				query += `DROP TABLE worker_pause_history;DROP TABLE worker_pause_state;`
			}
			if version < 4 {
				query += `DROP TABLE worker_primary_job_history;DROP TABLE worker_primary_jobs;`
			}
			if version < 3 {
				query += `DROP TABLE job_checklist_history;DROP TABLE job_checklists;DROP TABLE job_updates;`
			}
			if version < 2 {
				query += `DROP TABLE job_checkpoints;`
			}
			query += fmt.Sprintf(`UPDATE metadata SET value='%d' WHERE key='schema:jobs'`, version)
			if _, err := f.store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal(err)
			}
			if generationRead(t, destination, `SELECT value FROM metadata WHERE key='schema:jobs'`)[0][0].(string) != fmt.Sprint(version) || len(generationRead(t, destination, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('notes','note_revisions')`)) != 0 {
				t.Fatal("older restore migrated notes")
			}
		})
	}
}

func TestNotesBackupRejectsMalformedSchemaAndHistoricalState(t *testing.T) {
	f := newRestoreFixture(t)
	id := seedBackupNotes(t, f)
	for _, query := range []string{
		`DROP TABLE note_revisions`,
		`ALTER TABLE notes ADD COLUMN extra TEXT`,
		`UPDATE notes SET body=''`,
		`UPDATE note_revisions SET status_reason='' WHERE status='dismissed'`,
		`DELETE FROM note_revisions WHERE revision=2`,
		`DELETE FROM note_revisions WHERE revision=4`,
		`UPDATE note_revisions SET body='wrong latest current binding' WHERE revision=4`,
		`UPDATE note_revisions SET created_at='bad' WHERE revision=1`,
	} {
		t.Run(query, func(t *testing.T) {
			bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, bundle.Path, query)
			assertNotesBundleRejected(t, bundle.Path)
		})
	}
	for _, table := range []string{"notes", "note_revisions"} {
		for _, defect := range []string{"key", "type", "nullable", "default", "foreign-action"} {
			t.Run(table+"/"+defect, func(t *testing.T) {
				bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
				if err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", filepath.Join(bundle.Path, "firecontroller.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				var original string
				if err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&original); err != nil {
					db.Close()
					t.Fatal(err)
				}
				db.Close()
				changed := strings.Replace(original, "CREATE TABLE "+table, "CREATE TABLE altered_notes", 1)
				switch defect {
				case "key":
					if table == "notes" {
						changed = strings.Replace(changed, "PRIMARY KEY (project_id,id)", "PRIMARY KEY (project_id,body)", 1)
					} else {
						changed = strings.Replace(changed, "PRIMARY KEY (project_id,note_id,revision)", "PRIMARY KEY (project_id,note_id,revision,body)", 1)
					}
				case "type":
					changed = strings.Replace(changed, "revision INTEGER", "revision REAL", 1)
				case "nullable":
					changed = strings.Replace(changed, "body TEXT NOT NULL", "body TEXT", 1)
				case "default":
					changed = strings.Replace(changed, "phase TEXT NOT NULL", "phase TEXT NOT NULL DEFAULT ''", 1)
				case "foreign-action":
					changed = strings.Replace(changed, "REFERENCES workers(project_id,id)", "REFERENCES workers(project_id,id) ON UPDATE CASCADE", 1)
				}
				if changed == strings.Replace(original, "CREATE TABLE "+table, "CREATE TABLE altered_notes", 1) {
					t.Fatal("fixture did not introduce defect")
				}
				columns := strings.Join(strings.Fields(tables[table].columns), ",")
				mutateBundleSQL(t, bundle.Path, changed+`;INSERT INTO altered_notes SELECT `+columns+` FROM `+table+`;DROP TABLE `+table+`;ALTER TABLE altered_notes RENAME TO `+table)
				assertNotesBundleRejected(t, bundle.Path)
			})
		}
	}
	// A corrupt archive blocks operator backup, not an ordinary current-row read.
	if _, err := f.store.DB().Exec(`UPDATE note_revisions SET body='corrupt latest' WHERE note_id=? AND revision=4`, id); err != nil {
		t.Fatal(err)
	}
	f.request(t, "GET", "/api/v1/projects/"+f.project+"/notes/"+id, nil)
	_, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "refused"))
	backupCode(t, err, "backup_invalid")
}
func assertNotesBundleRejected(t *testing.T, path string) {
	t.Helper()
	before := restoreTree(t, path)
	_, err := Inspect(t.Context(), path)
	backupCode(t, err, "backup_invalid")
	destination := filepath.Join(t.TempDir(), "refused")
	_, err = Restore(t.Context(), path, destination)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid notes created restore destination", err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, path)) {
		t.Fatal("rejected bundle was rewritten")
	}
}

func TestNotesBackupAcceptsStructuralTimestampsAfterRollback(t *testing.T) {
	f := newRestoreFixture(t)
	id := seedBackupNotes(t, f)
	earlier := "2000-01-01T00:00:00.100000000Z"
	if _, err := f.store.DB().Exec(`UPDATE notes SET updated_at=? WHERE id=?`, earlier, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE note_revisions SET updated_at=?,recorded_at=? WHERE note_id=? AND revision=4`, earlier, earlier, id); err != nil {
		t.Fatal(err)
	}
	bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal("rollback did not invalidate logical history", err)
	}
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	if generationRead(t, destination, `SELECT updated_at FROM notes`)[0][0].(string) != earlier {
		t.Fatal("restore normalized historical time")
	}
}
