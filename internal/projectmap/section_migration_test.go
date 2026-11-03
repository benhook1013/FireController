package projectmap_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/projectmap"
)

// Exact published pre-section migrations. Start at authentic v2, rather than
// deleting metadata from a v3 schema and calling that an upgrade fixture.
var preSectionMigrations = [][]string{{
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

func TestMapSectionMigrationPreservesVersionTwoRowsAndRestarts(t *testing.T) {
	f := setupWithMapSchema(t, func(ctx context.Context, s *core.Store) error {
		return s.EnsureSchema(ctx, "project_map", preSectionMigrations...)
	})
	doc := importDocument()
	preview := f.previewImport(doc)
	f.applyImport(preview, "pre-section-import")
	record := f.importRecord(0)
	id := record.Mappings[0].EntryID
	v := doc.Entries[0].Entry
	v.Title = "Manual retained edit"
	expect(t, f.replace(id, "pre-section-edit", 1, v), 200)
	w := f.call("POST", "/api/v1/projects/"+f.project+"/jobs", f.token, "linked-job", map[string]string{"title": "Linked job", "brief": "Unchanged"})
	expect(t, w, 201)
	var job struct {
		ID string `json:"id"`
	}
	decode(t, w, &job)
	expect(t, f.call("PUT", "/api/v1/projects/"+f.project+"/jobs/"+job.ID+"/workstream", f.token, "pre-section-link", map[string]any{"expected_revision": 0, "workstream_id": id}), 200)
	before := f.importState()
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	after := f.importState()
	// Metadata advances; all application rows retain their exact values/JSON.
	delete(before, "metadata")
	delete(after, "metadata")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("section migration changed retained data")
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil || version != "3" {
		t.Fatal(version, err)
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.DB().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("migration broke FK graph")
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	unchanged := f.importState()
	if err = projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged, f.importState()) {
		t.Fatal("repeated startup was not inert")
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = reopened
	f.mount()
	if err = projectmap.EnsureSchema(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged, f.importState()) {
		t.Fatal("restart changed data")
	}
	section := projectmap.Editable{Kind: "section", Title: "Added after upgrade", Description: "Markdown **text**"}
	f.create("section-after-migration", section)
}

func TestMapSectionMigrationFailureRollsBackVersionTwo(t *testing.T) {
	f := setupWithMapSchema(t, func(ctx context.Context, s *core.Store) error {
		return s.EnsureSchema(ctx, "project_map", preSectionMigrations...)
	})
	original := f.create("before-failure", workstream())
	// A corrupt old row must fail the new CHECK on reinsert. Dropped tables,
	// cascades, temp copies and metadata must all roll back together.
	if _, err := f.store.DB().Exec(`PRAGMA ignore_check_constraints=ON; UPDATE project_map_entries SET kind='invalid' WHERE id=?; PRAGMA ignore_check_constraints=OFF`, original.ID); err != nil {
		t.Fatal(err)
	}
	before := f.importState()
	if err := projectmap.EnsureSchema(t.Context(), f.store); err == nil {
		t.Fatal("invalid old data migrated")
	}
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("failed migration changed old data")
	}
	var ddl string
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='project_map_entries'`).Scan(&ddl); err != nil || strings.Contains(ddl, "'section'") {
		t.Fatal("failed migration changed old schema", err)
	}
	// Repair the deliberately corrupt fixture, then retry the same migration.
	if _, err := f.store.DB().Exec(`UPDATE project_map_entries SET kind='workstream' WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}
