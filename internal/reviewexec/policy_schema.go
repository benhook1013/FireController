package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsurePolicySchema installs project policy storage independently of execution.
func EnsurePolicySchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchemaWithBackfill(ctx, "review_policy", [][]string{{
		`CREATE TABLE review_policy_config(project_id TEXT NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),body_json TEXT NOT NULL,actor_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,revision))`,
	}, {}}, migratePolicyChannelPauses)
}
