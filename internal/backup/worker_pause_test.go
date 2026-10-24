package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWorkerPauseBackupRoundTripPreservesCoordination(t *testing.T) {
	f := newRestoreFixture(t)
	base := "/api/v1/projects/" + f.project + "/workers/" + f.worker
	job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	f.request(t, "PUT", base+"/primary-job", map[string]any{"expected_revision": 0, "job_id": job})
	for revision, state := range []struct {
		paused bool
		reason string
	}{{true, "Private pause <literal> 雪\n\t"}, {true, "Corrected reason"}, {false, ""}, {true, "Retained pause"}} {
		f.request(t, "PUT", base+"/pause", map[string]any{"expected_revision": revision, "paused": state.paused, "reason": state.reason})
	}
	tables := []string{"worker_pause_state", "worker_pause_history", "worker_primary_jobs", "worker_primary_job_history", "jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "inbox_messages", "inbox_recipient_state", "review_policy_config", "review_execution_config", "review_operations", "review_attempt_versions", "review_attempt_memberships", "review_current_attempts", "receipts", "events"}
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
		t.Fatal("jobs5 inspect", err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	result, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil {
		t.Fatal("jobs5 restore", err)
	}
	if result.State != "inactive" || result.InstanceID == f.store.InstanceID() {
		t.Fatal(result)
	}
	for _, table := range tables {
		if !reflect.DeepEqual(original[table], generationRead(t, destination, `SELECT * FROM `+table+` ORDER BY rowid`)) {
			t.Fatal("restore rewrote pause or unrelated coordination", table)
		}
		if !reflect.DeepEqual(original[table], restoreRows(t, f.store.DB(), `SELECT * FROM `+table+` ORDER BY rowid`)) {
			t.Fatal("backup mutated source", table)
		}
	}
	if got := generationRead(t, destination, `SELECT count(*) FROM credentials WHERE revoked=0`)[0][0].(int64); got != 0 {
		t.Fatal("restored credentials revived", got)
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("immutable bundle mutated")
	}
}

func TestWorkerPauseOlderJobsBundlesRemainSupported(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newRestoreFixture(t)
			if version == 4 {
				job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
				f.request(t, "PUT", "/api/v1/projects/"+f.project+"/workers/"+f.worker+"/primary-job", map[string]any{"expected_revision": 0, "job_id": job})
			}
			sql := `DROP TABLE job_progress_history; DROP TABLE job_progress; DROP TABLE job_summary_history; DROP TABLE job_summaries; DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history;DROP TABLE worker_pause_state;`
			if version < 4 {
				sql += `DROP TABLE worker_primary_job_history;DROP TABLE worker_primary_jobs;`
			}
			if version < 3 {
				sql += `DROP TABLE job_updates;DROP TABLE job_checklists;DROP TABLE job_checklist_history;`
			}
			if version < 2 {
				sql += `DROP TABLE job_checkpoints;`
			}
			sql += fmt.Sprintf(`UPDATE metadata SET value='%d' WHERE key='schema:jobs'`, version)
			if _, err := f.store.DB().Exec(sql); err != nil {
				t.Fatal(err)
			}
			original := restoreRows(t, f.store.DB(), `SELECT * FROM metadata ORDER BY key`)
			var focus [][]any
			if version == 4 {
				focus = restoreRows(t, f.store.DB(), `SELECT * FROM worker_primary_jobs`)
			}
			bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal("older create", err)
			}
			immutable := restoreTree(t, bundle.Path)
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("older inspect", err)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal("older restore", err)
			}
			got := generationRead(t, destination, `SELECT value FROM metadata WHERE key='schema:jobs'`)[0][0].(string)
			if got != fmt.Sprint(version) {
				t.Fatal("old schema migrated during restore", got)
			}
			if rows := generationRead(t, destination, `SELECT name FROM sqlite_schema WHERE type='table' AND name LIKE 'worker_pause_%'`); len(rows) != 0 {
				t.Fatal("old restore synthesized pause")
			}
			if version == 4 && !reflect.DeepEqual(focus, generationRead(t, destination, `SELECT * FROM worker_primary_jobs`)) {
				t.Fatal("jobs4 focus changed")
			}
			if !reflect.DeepEqual(original, restoreRows(t, f.store.DB(), `SELECT * FROM metadata ORDER BY key`)) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("older source/bundle mutated")
			}
		})
	}
}

func TestWorkerPauseBackupRejectsNoncanonicalStateAndHistory(t *testing.T) {
	f := newRestoreFixture(t)
	f.request(t, "PUT", "/api/v1/projects/"+f.project+"/workers/"+f.worker+"/pause", map[string]any{"expected_revision": 0, "paused": true, "reason": "Retained private reason"})
	current := `project_id TEXT NOT NULL,worker_id TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),paused INTEGER NOT NULL CHECK(paused IN(0,1)),reason TEXT NOT NULL CHECK(paused=1 OR reason=''),PRIMARY KEY(project_id,worker_id)`
	history := `project_id TEXT NOT NULL,worker_id TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),paused INTEGER NOT NULL CHECK(paused IN(0,1)),reason TEXT NOT NULL CHECK(paused=1 OR reason=''),actor_id TEXT NOT NULL,actor_worker_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,worker_id,revision)`
	fk := `,FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id)`
	for _, table := range []string{"worker_pause_state", "worker_pause_history"} {
		columns, key, moved := current, `PRIMARY KEY(project_id,worker_id)`, `PRIMARY KEY(project_id,reason)`
		if table == "worker_pause_history" {
			columns, key, moved = history, `PRIMARY KEY(project_id,worker_id,revision)`, `PRIMARY KEY(project_id,worker_id,paused)`
		}
		for _, test := range []struct{ name, columns, foreign string }{
			{"missing-key", strings.Replace(columns, ","+key, "", 1), fk},
			{"moved-key", strings.Replace(columns, key, moved, 1), fk},
			{"wrong-boolean-type", strings.Replace(columns, "paused INTEGER", "paused REAL", 1), fk},
			{"nullable-reason", strings.Replace(columns, "reason TEXT NOT NULL", "reason TEXT", 1), fk},
			{"unexpected-default", strings.Replace(columns, "paused INTEGER NOT NULL", "paused INTEGER NOT NULL DEFAULT 0", 1), fk},
			{"extra-column", columns, fk},
			{"missing-table", columns, fk},
			{"wrong-foreign-action", columns, fk + " ON DELETE CASCADE"},
		} {
			t.Run(table+"/"+test.name, func(t *testing.T) {
				bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
				if err != nil {
					t.Fatal(err)
				}
				switch test.name {
				case "extra-column":
					mutateBundleSQL(t, bundle.Path, `ALTER TABLE `+table+` ADD COLUMN extra TEXT`)
				case "missing-table":
					mutateBundleSQL(t, bundle.Path, `DROP TABLE `+table)
				default:
					mutateBundleSQL(t, bundle.Path, `CREATE TABLE altered_pause (`+test.columns+test.foreign+`);INSERT INTO altered_pause SELECT `+strings.Join(strings.Fields(tables[table].columns), ",")+` FROM `+table+`;DROP TABLE `+table+`;ALTER TABLE altered_pause RENAME TO `+table)
				}
				immutable := restoreTree(t, bundle.Path)
				_, err = Inspect(t.Context(), bundle.Path)
				backupCode(t, err, "backup_invalid")
				destination := filepath.Join(t.TempDir(), "refused")
				_, err = Restore(t.Context(), bundle.Path, destination)
				backupCode(t, err, "backup_invalid")
				if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid schema created restore destination", err)
				}
				if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
					t.Fatal("refused schema modified bundle")
				}
			})
		}
	}
}
