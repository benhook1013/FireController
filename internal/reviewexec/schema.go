package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_execution", []string{
		`CREATE TABLE review_execution_config(project_id TEXT NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),body_json TEXT NOT NULL,PRIMARY KEY(project_id,revision))`,
		`CREATE TABLE review_operations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,configuration_revision INTEGER NOT NULL,pull_number INTEGER NOT NULL,channel TEXT NOT NULL,head_sha TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,reason TEXT NOT NULL,attempt_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision))`,
		`CREATE INDEX review_operations_pending ON review_operations(state,created_at,id)`,
		`CREATE TABLE review_execution_attempts(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL UNIQUE REFERENCES review_operations(id),project_id TEXT NOT NULL REFERENCES projects(id),provider TEXT NOT NULL,account TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,anchor_json TEXT NOT NULL,intent TEXT NOT NULL,external_id TEXT NOT NULL)`,
		`CREATE TABLE review_execution_reservations(provider TEXT NOT NULL,repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,action_class TEXT NOT NULL,account TEXT NOT NULL,attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),PRIMARY KEY(provider,repository_id,pull_number,action_class),UNIQUE(provider,account,action_class))`,
		`CREATE TABLE review_execution_transitions(attempt_id TEXT NOT NULL REFERENCES review_execution_attempts(id),revision INTEGER NOT NULL,state TEXT NOT NULL,reason TEXT NOT NULL,external_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(attempt_id,revision))`,
	}, []string{`ALTER TABLE review_operations ADD COLUMN capability_fingerprint TEXT NOT NULL DEFAULT ''`})
}
