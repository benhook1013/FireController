package backup

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

// Exact published pre-section DDL, retained to prove older bundles remain
// admissible without relabeling the new widened-CHECK schema as version two.
var backupMapVersionTwoMigrations = [][]string{{
	`CREATE TABLE project_map_entries (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('workstream','return_point')),
 title TEXT NOT NULL,
 state TEXT NOT NULL,
 archived INTEGER NOT NULL CHECK(archived IN (0,1)),
 revision INTEGER NOT NULL CHECK(revision>=1),
 state_json TEXT NOT NULL,
 PRIMARY KEY(project_id,id),
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
 );`,
	`CREATE INDEX project_map_entries_order ON project_map_entries(project_id,archived,id);`,
	`CREATE INDEX project_map_entries_kind_order ON project_map_entries(project_id,archived,kind,id);`,
	`CREATE TABLE project_map_entry_history (
 project_id TEXT NOT NULL,
 entry_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 actor_id TEXT NOT NULL,
 state_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,entry_id,revision),
 FOREIGN KEY(project_id,entry_id) REFERENCES project_map_entries(project_id,id) ON DELETE CASCADE
 );`,
	`CREATE TABLE job_workstreams (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 workstream_id TEXT,
 workstream_title TEXT NOT NULL,
 PRIMARY KEY(project_id,job_id),
 FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE,
 FOREIGN KEY(project_id,workstream_id) REFERENCES project_map_entries(project_id,id)
 );`,
	`CREATE TABLE job_workstream_history (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 workstream_id TEXT,
 workstream_title TEXT NOT NULL,
 actor_id TEXT NOT NULL,
 job_revision INTEGER NOT NULL CHECK(job_revision>=1),
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,job_id,revision),
 FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE,
 FOREIGN KEY(project_id,workstream_id) REFERENCES project_map_entries(project_id,id)
 );`,
}, {
	`CREATE TABLE project_map_import_sources (
 project_id TEXT NOT NULL,
 source_key TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 record_json TEXT NOT NULL,
 PRIMARY KEY(project_id,source_key),
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
 );
 CREATE TABLE project_map_import_history (
 project_id TEXT NOT NULL,
 source_key TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 record_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,source_key,revision),
 FOREIGN KEY(project_id,source_key) REFERENCES project_map_import_sources(project_id,source_key) ON DELETE CASCADE
 );
 CREATE TABLE project_map_import_items (
 project_id TEXT NOT NULL,
 source_key TEXT NOT NULL,
 item_key TEXT NOT NULL,
 entry_id TEXT NOT NULL,
 entry_revision INTEGER NOT NULL CHECK(entry_revision>=1),
 source_revision INTEGER NOT NULL CHECK(source_revision>=1),
 PRIMARY KEY(project_id,source_key,item_key),
 FOREIGN KEY(project_id,source_key) REFERENCES project_map_import_sources(project_id,source_key) ON DELETE CASCADE,
 FOREIGN KEY(project_id,entry_id,entry_revision) REFERENCES project_map_entry_history(project_id,entry_id,revision),
 FOREIGN KEY(project_id,source_key,source_revision) REFERENCES project_map_import_history(project_id,source_key,revision)
 );`}}

func TestProjectMapVersionTwoBundleRemainsCanonical(t *testing.T) {
	f := newRestoreFixture(t)
	if err := f.store.EnsureSchema(t.Context(), "project_map", backupMapVersionTwoMigrations...); err != nil {
		t.Fatal(err)
	}
	projectmap.Register(f.handler.(*http.ServeMux), f.store)
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/map/entries", projectmap.Editable{Kind: "workstream", Title: "Old canonical map", Description: "Original bytes"})
	before := restoreRows(t, f.store.DB(), `SELECT * FROM project_map_entries ORDER BY project_id,id`)
	bundle := restoreBundle(t, f)
	sealed := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil || version != "2" {
		t.Fatal(version, err)
	}
	if !reflect.DeepEqual(before, restoreRows(t, db, `SELECT * FROM project_map_entries ORDER BY project_id,id`)) || !reflect.DeepEqual(sealed, restoreTree(t, bundle.Path)) {
		t.Fatal("older backup changed")
	}
}

func TestProjectMapEditorialBundleRetainsSectionsPositionsAndImportHistory(t *testing.T) {
	f := mapImportRestoreFixture(t)
	base := "/api/v1/projects/" + f.project + "/map/entries"
	section := projectmap.Editable{Kind: "section", Title: "Narrative", Description: "# Literal Markdown <tag>", Position: 19}
	created := f.request(t, "POST", base, section)
	id := created["id"].(string)
	section.Position = 2
	section.Description = "Updated **Markdown**"
	f.request(t, "PUT", base+"/"+id, map[string]any{"expected_revision": 1, "entry": section})
	queries := map[string]string{
		"entries":    `SELECT * FROM project_map_entries ORDER BY project_id,id`,
		"history":    `SELECT * FROM project_map_entry_history ORDER BY project_id,entry_id,revision`,
		"sources":    `SELECT * FROM project_map_import_sources ORDER BY project_id,source_key`,
		"provenance": `SELECT * FROM project_map_import_history ORDER BY project_id,source_key,revision`,
		"bindings":   `SELECT * FROM project_map_import_items ORDER BY project_id,source_key,item_key`,
		"jobs":       `SELECT * FROM jobs ORDER BY project_id,id`,
		"messages":   `SELECT * FROM inbox_messages ORDER BY sequence`,
	}
	before := map[string][][]any{}
	for name, query := range queries {
		before[name] = restoreRows(t, f.store.DB(), query)
	}
	bundle := restoreBundle(t, f)
	sealed := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, query := range queries {
		if !reflect.DeepEqual(before[name], restoreRows(t, db, query)) || !reflect.DeepEqual(before[name], restoreRows(t, f.store.DB(), query)) {
			t.Fatal("map section/import/private state changed", name)
		}
	}
	var version, state string
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil || version != "3" {
		t.Fatal(version, err)
	}
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='restore:state'`).Scan(&state); err != nil || state != "inactive" {
		t.Fatal(state, err)
	}
	if !reflect.DeepEqual(sealed, restoreTree(t, bundle.Path)) {
		t.Fatal("sealed input mutated")
	}
}

func TestProjectMapEditorialBundleRejectsResealedVersionThreeSchemaDamage(t *testing.T) {
	f, _, _ := mapRestoreFixture(t)
	for name, change := range map[string]string{
		"old-kind-check":          `PRAGMA writable_schema=ON;UPDATE sqlite_schema SET sql=replace(sql,'''workstream'',''return_point'',''section''','''workstream'',''return_point''') WHERE name='project_map_entries'`,
		"missing-editorial-index": `DROP INDEX project_map_entries_editorial_order`,
		"wrong-editorial-index":   `DROP INDEX project_map_entries_editorial_order;CREATE INDEX project_map_entries_editorial_order ON project_map_entries(project_id,archived,id)`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, change)
			before := restoreTree(t, bundle.Path)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			target := filepath.Join(t.TempDir(), "must-not-exist")
			_, err = Restore(t.Context(), bundle.Path, target)
			backupCode(t, err, "backup_invalid")
			if _, err = os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("invalid schema created destination", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("schema refusal mutated bundle")
			}
		})
	}
}
