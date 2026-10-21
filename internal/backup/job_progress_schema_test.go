package backup

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJobProgressBackupRejectsNoncanonicalSchema(t *testing.T) {
	f := newRestoreFixture(t)
	job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	base := "/api/v1/projects/" + f.project + "/jobs/" + job
	f.request(t, "PATCH", base, map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": "Retained progress"}})
	f.request(t, "PUT", base+"/checklist", map[string]any{"expected_job_revision": 2, "expected_revision": 0, "items": []map[string]any{{"id": "retained", "text": "Retained item", "done": false}}})
	for _, table := range []string{"job_updates", "job_checklists", "job_checklist_history"} {
		defects := []string{"missing-primary-key", "wrong-revision-type", "nullable-actor", "unexpected-default"}
		if table == "job_updates" {
			defects = append(defects, "missing-autoincrement", "descending-primary-key", "missing-id-unique", "composite-id-unique", "partial-id-unique", "expression-id-unique")
		} else {
			defects = append(defects, "moved-primary-key")
		}
		for _, defect := range defects {
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
				err = db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&original)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				definition := strings.Replace(original, "CREATE TABLE "+table, "CREATE TABLE altered_progress", 1)
				key := "PRIMARY KEY (project_id,job_id)"
				if table == "job_checklist_history" {
					key = "PRIMARY KEY (project_id,job_id,revision)"
				}
				changed, extra := definition, ""
				switch defect {
				case "missing-primary-key":
					if table == "job_updates" {
						changed = strings.Replace(changed, "sequence INTEGER PRIMARY KEY AUTOINCREMENT", "sequence INTEGER", 1)
					} else {
						changed = strings.Replace(changed, key+",", "", 1)
					}
				case "moved-primary-key":
					changed = strings.Replace(changed, key, strings.TrimSuffix(key, ")")+",actor_id)", 1)
				case "wrong-revision-type":
					changed = strings.Replace(changed, "job_revision INTEGER", "job_revision REAL", 1)
				case "nullable-actor":
					changed = strings.Replace(changed, "actor_id TEXT NOT NULL", "actor_id TEXT", 1)
				case "unexpected-default":
					changed = strings.Replace(changed, "worker_id TEXT NOT NULL", "worker_id TEXT NOT NULL DEFAULT ''", 1)
				case "missing-autoincrement":
					changed = strings.Replace(changed, "PRIMARY KEY AUTOINCREMENT", "PRIMARY KEY", 1)
				case "descending-primary-key":
					changed = strings.Replace(changed, "PRIMARY KEY AUTOINCREMENT", "PRIMARY KEY DESC", 1)
				case "missing-id-unique", "composite-id-unique", "partial-id-unique", "expression-id-unique":
					changed = strings.Replace(changed, "id TEXT NOT NULL UNIQUE", "id TEXT NOT NULL", 1)
					switch defect {
					case "composite-id-unique":
						extra = `;CREATE UNIQUE INDEX altered_identity ON job_updates(id,job_id)`
					case "partial-id-unique":
						extra = `;CREATE UNIQUE INDEX altered_identity ON job_updates(id) WHERE job_revision=1`
					case "expression-id-unique":
						extra = `;CREATE UNIQUE INDEX altered_identity ON job_updates(lower(id))`
					}
				}
				if changed == definition {
					t.Fatal("fixture did not change schema")
				}
				columns := strings.Join(strings.Fields(tables[table].columns), ",")
				mutateBundleSQL(t, bundle.Path, changed+`;INSERT INTO altered_progress SELECT `+columns+` FROM `+table+`;DROP TABLE `+table+`;ALTER TABLE altered_progress RENAME TO `+table+extra)
				before := restoreTree(t, bundle.Path)
				_, err = Inspect(t.Context(), bundle.Path)
				backupCode(t, err, "backup_invalid")
				destination := filepath.Join(t.TempDir(), "must-not-exist")
				_, err = Restore(t.Context(), bundle.Path, destination)
				backupCode(t, err, "backup_invalid")
				if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid schema created restore destination", err)
				}
				if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
					t.Fatal("inspection or restore modified malformed bundle")
				}
			})
		}
	}
}
