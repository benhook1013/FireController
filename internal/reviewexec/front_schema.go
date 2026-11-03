package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureFrontSchema(ctx context.Context, store *core.Store) error {
	if err := store.EnsureSchema(ctx, "review_fronts", FrontSchemaSQL()); err != nil {
		return err
	}
	return store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error { return ValidateFrontSnapshot(ctx, tx) })
}

// FrontSchemaSQL is independent canonical DDL for the optional current cache.
func FrontSchemaSQL() []string {
	return []string{`CREATE TABLE review_front_frames(
 project_id TEXT NOT NULL REFERENCES projects(id),channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),
 repository_id INTEGER NOT NULL CHECK(repository_id>0),configuration_revision INTEGER NOT NULL CHECK(configuration_revision>0),
 checked_at TEXT NOT NULL,source_observed_at TEXT NOT NULL,complete INTEGER NOT NULL CHECK(complete IN (0,1)),source_reason TEXT NOT NULL,prefix_json TEXT NOT NULL,
 PRIMARY KEY(project_id,channel),FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision))`}
}
