package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

// EnsureSourceRefreshSchema owns source-only operations independently of review
// attempts. Observing source never creates an external action reservation.
func EnsureSourceRefreshSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_source_refresh", []string{
		`CREATE TABLE review_source_refreshes(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),repository_id INTEGER NOT NULL,actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,configuration_revision INTEGER NOT NULL,capability_id TEXT NOT NULL,capability_fingerprint TEXT NOT NULL,pull_number INTEGER NOT NULL,head_sha TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,reason TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,observation_json TEXT NOT NULL,FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision))`,
		`CREATE INDEX review_source_refresh_pending ON review_source_refreshes(state,created_at,id)`,
		`CREATE INDEX review_source_refresh_project_pending ON review_source_refreshes(project_id,state) WHERE state IN ('queued','running')`,
		`CREATE INDEX review_source_refresh_target ON review_source_refreshes(project_id,pull_number,state,created_at DESC,id DESC)`,
		`CREATE INDEX review_source_refresh_coalesce ON review_source_refreshes(project_id,repository_id,pull_number,head_sha,configuration_revision,capability_fingerprint,actor_id,worker_id,state) WHERE state IN ('queued','running')`,
	}, []string{
		`ALTER TABLE review_source_refreshes ADD COLUMN include_ci INTEGER NOT NULL DEFAULT 0 CHECK(include_ci IN (0,1))`,
		`ALTER TABLE review_source_refreshes ADD COLUMN ci_observation_json TEXT NOT NULL DEFAULT 'null'`,
		`DROP INDEX review_source_refresh_coalesce`,
		`CREATE INDEX review_source_refresh_coalesce ON review_source_refreshes(project_id,repository_id,pull_number,head_sha,configuration_revision,capability_fingerprint,actor_id,worker_id,include_ci,state) WHERE state IN ('queued','running')`,
		`CREATE INDEX review_source_refresh_ci_target ON review_source_refreshes(project_id,pull_number,state,include_ci,created_at DESC,id DESC)`,
	}, []string{
		`ALTER TABLE review_source_refreshes ADD COLUMN include_readiness INTEGER NOT NULL DEFAULT 0 CHECK(include_readiness IN (0,1))`,
		`ALTER TABLE review_source_refreshes ADD COLUMN review_state_json TEXT NOT NULL DEFAULT 'null'`,
		`DROP INDEX review_source_refresh_coalesce`,
		`CREATE INDEX review_source_refresh_coalesce ON review_source_refreshes(project_id,repository_id,pull_number,head_sha,configuration_revision,capability_fingerprint,actor_id,worker_id,include_ci,include_readiness,state) WHERE state IN ('queued','running')`,
		`CREATE INDEX review_source_refresh_ci_latest ON review_source_refreshes(project_id,pull_number,created_at DESC,id DESC) WHERE state='completed' AND (include_ci=1 OR include_readiness=1)`,
		`CREATE INDEX review_source_refresh_readiness ON review_source_refreshes(project_id,pull_number,created_at DESC,id DESC) WHERE state='completed' AND include_readiness=1`,
	})
}
