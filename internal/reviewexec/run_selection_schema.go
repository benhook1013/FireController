package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSelectionSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_selection", SelectionSchemaSQL())
}

// SelectionSchemaSQL returns an independent canonical DDL slice.
func SelectionSchemaSQL() []string {
	return []string{
		`CREATE TABLE review_selections(
 id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),
 configuration_revision INTEGER NOT NULL CHECK(configuration_revision>=1),policy_revision INTEGER NOT NULL CHECK(policy_revision>=1),capability_fingerprint TEXT NOT NULL,
 expected_pull_number INTEGER NOT NULL CHECK(expected_pull_number BETWEEN 0 AND 2147483647),state TEXT NOT NULL CHECK(state IN ('queued','selecting','selected','held','empty')),
 reason TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),operation_id TEXT UNIQUE REFERENCES review_operations(id),created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 CHECK((state='selected' AND operation_id IS NOT NULL) OR (state<>'selected' AND operation_id IS NULL)),
 FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision),FOREIGN KEY(project_id,policy_revision) REFERENCES review_policy_config(project_id,revision))`,
		`CREATE UNIQUE INDEX review_selections_pending_channel ON review_selections(project_id,channel) WHERE state IN ('queued','selecting')`,
		`CREATE INDEX review_selections_pending ON review_selections(state,created_at,id)`,
	}
}
