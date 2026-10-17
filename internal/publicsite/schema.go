package publicsite

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "public_site", []string{
		`CREATE TABLE project_publications (
 project_id TEXT PRIMARY KEY NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 slug TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 manifest_json TEXT NOT NULL,
 round_salt TEXT NOT NULL,
 generation TEXT NOT NULL,
 public_revision INTEGER NOT NULL CHECK(public_revision>=0),
 public_updated_at TEXT NOT NULL,
 refresh_error TEXT NOT NULL,
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
 );`,
		`CREATE TABLE public_project_snapshots (
 project_id TEXT NOT NULL,
 generation TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 content_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,generation,revision),
 FOREIGN KEY(project_id) REFERENCES project_publications(project_id) ON DELETE CASCADE
 );`,
	})
}
