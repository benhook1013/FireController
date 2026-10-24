// Package projectmap implements project-scoped editorial maps and independent job references.
package projectmap

import (
	"context"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

const mapEntriesVersionOneDDL = `CREATE TABLE project_map_entries (
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
 );`
const editorialIndexDDL = `CREATE INDEX project_map_entries_editorial_order ON project_map_entries(project_id,archived,COALESCE(json_extract(state_json,'$.position'),0),id);`

func mapEntriesSectionDDL() string {
	return strings.Replace(mapEntriesVersionOneDDL, "CHECK(kind IN ('workstream','return_point'))", "CHECK(kind IN ('workstream','return_point','section'))", 1)
}

// EnsureSchema runs during exclusive startup, after jobs.EnsureSchema.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	migrations := [][]string{{
		mapEntriesVersionOneDDL,
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
 );`,
	}}
	migrations = append(migrations, sectionMigration(migrations[0]))
	return store.EnsureSchema(ctx, "project_map", migrations...)
}

// SQLite cannot widen a CHECK in place. Keep every dependent row in private
// temporary tables before dropping the FK children, then rebuild with FKs on.
// EnsureSchema executes this entire group under one exclusive transaction.
func sectionMigration(versionOne []string) []string {
	tables := []string{"project_map_entries", "project_map_entry_history", "job_workstreams", "job_workstream_history", "project_map_import_items"}
	statements := make([]string, 0, 32)
	for _, table := range tables {
		statements = append(statements, "CREATE TEMP TABLE section_copy_"+table+" AS SELECT * FROM "+table)
	}
	for _, table := range []string{"project_map_import_items", "job_workstream_history", "job_workstreams", "project_map_entry_history", "project_map_entries"} {
		statements = append(statements, "DROP TABLE "+table)
	}
	for _, statement := range versionOne {
		if statement == mapEntriesVersionOneDDL {
			statement = mapEntriesSectionDDL()
		}
		statements = append(statements, statement)
	}
	statements = append(statements, `CREATE TABLE project_map_import_items (
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
 );`)
	for _, table := range tables {
		statements = append(statements, "INSERT INTO "+table+" SELECT * FROM section_copy_"+table, "DROP TABLE section_copy_"+table)
	}
	statements = append(statements, editorialIndexDDL)
	return statements
}
