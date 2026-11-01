package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

// SchedulingSchemaSQL owns optional new-request controls, not execution bindings
// or provider evidence. Existing installations start with no enabled settings.
func SchedulingSchemaSQL() []string {
	return []string{
		`CREATE TABLE review_scheduling(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),automatic_hosted INTEGER NOT NULL CHECK(automatic_hosted IN(0,1)),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,credential_id TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE review_pull_scheduling(project_id TEXT NOT NULL REFERENCES projects(id),pull_number INTEGER NOT NULL CHECK(pull_number BETWEEN 1 AND 2147483647),revision INTEGER NOT NULL CHECK(revision>0),paused INTEGER NOT NULL CHECK(paused IN(0,1)),reason TEXT NOT NULL,actor_id TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(project_id,pull_number))`,
		`CREATE TABLE review_automatic_selections(selection_id TEXT PRIMARY KEY REFERENCES review_selections(id),project_id TEXT NOT NULL REFERENCES projects(id),scheduling_revision INTEGER NOT NULL CHECK(scheduling_revision>0),basis_sha256 TEXT NOT NULL CHECK(length(basis_sha256)=64),settled_basis_sha256 TEXT NOT NULL DEFAULT '',pull_number INTEGER NOT NULL DEFAULT 0 CHECK(pull_number BETWEEN 0 AND 2147483647),pull_basis_sha256 TEXT NOT NULL DEFAULT '',actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,credential_id TEXT NOT NULL,UNIQUE(project_id,basis_sha256))`,
		`CREATE INDEX review_automatic_selections_settled ON review_automatic_selections(project_id,settled_basis_sha256)`,
		`CREATE INDEX review_automatic_selections_pull ON review_automatic_selections(project_id,pull_number,pull_basis_sha256)`,
	}
}
func EnsureSchedulingSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_scheduling", SchedulingSchemaSQL())
}
