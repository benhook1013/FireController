// Package presentation owns provider-free project appearance and immutable history.
package presentation

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "project_presentation", []string{
		`CREATE TABLE project_presentations (
 project_id TEXT PRIMARY KEY NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 state_json TEXT NOT NULL,
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
 );`,
		`CREATE TABLE project_presentation_history (
 project_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 actor_id TEXT NOT NULL,
 state_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,revision),
 FOREIGN KEY(project_id) REFERENCES project_presentations(project_id) ON DELETE CASCADE
 );`,
	})
}
