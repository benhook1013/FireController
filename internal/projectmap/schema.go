// Package projectmap implements project-scoped editorial maps and independent job references.
package projectmap

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

// EnsureSchema runs during exclusive startup, after jobs.EnsureSchema.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "project_map", []string{
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
	})
}
