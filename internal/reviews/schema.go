package reviews

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureSchema creates the fixed reviews read-model schema during startup.
// Request handlers never perform DDL or compatibility migrations.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "reviews", []string{
		`CREATE TABLE IF NOT EXISTS review_snapshots (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 snapshot_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 repository_id INTEGER NOT NULL CHECK (repository_id > 0),
 source TEXT NOT NULL,
 observed_at TEXT NOT NULL,
 recorded_at TEXT NOT NULL,
 payload_sha256 TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id),
 UNIQUE (project_id, revision)
);`,
		`CREATE TABLE IF NOT EXISTS review_workspace (
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 snapshot_id TEXT NOT NULL,
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id)
);`,
		`CREATE TABLE IF NOT EXISTS review_pulls (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 title TEXT NOT NULL,
 state TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 base_ref TEXT NOT NULL,
 base_sha TEXT NOT NULL,
 ci_state TEXT NOT NULL,
 mergeability TEXT NOT NULL,
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 run_count INTEGER NOT NULL CHECK (run_count >= 0),
 attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
 recent_runs_json TEXT NOT NULL,
 latest_attempts_json TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, number),
 UNIQUE (project_id, snapshot_id, position),
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_pulls_snapshot_position
 ON review_pulls(project_id, snapshot_id, position);`,
		`CREATE TABLE IF NOT EXISTS review_runs (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 run_order INTEGER NOT NULL CHECK (run_order >= 0),
 id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_run_id TEXT NOT NULL,
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 reviewer TEXT NOT NULL,
 scope TEXT NOT NULL,
 outcome TEXT NOT NULL,
 attributable INTEGER NOT NULL CHECK (attributable IN (0,1)),
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 found INTEGER NOT NULL CHECK (found >= 0),
 accepted INTEGER NOT NULL CHECK (accepted >= 0),
 routed INTEGER NOT NULL CHECK (routed >= 0),
 finalized INTEGER NOT NULL CHECK (finalized IN (0,1)),
 fixed INTEGER NOT NULL CHECK (fixed >= 0),
	coverage_limits_json TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, pull_number, id),
 UNIQUE (project_id, snapshot_id, pull_number, run_order),
 UNIQUE (project_id, snapshot_id, source, source_run_id),
 FOREIGN KEY (project_id, snapshot_id, pull_number)
  REFERENCES review_pulls(project_id, snapshot_id, number) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_runs_project_source_id
 ON review_runs(project_id, source, source_run_id, finalized);`,
		`CREATE INDEX IF NOT EXISTS review_runs_snapshot_pull_order
 ON review_runs(project_id, snapshot_id, pull_number, run_order DESC);`,
		`CREATE TABLE IF NOT EXISTS review_findings (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 run_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 source_finding_id TEXT NOT NULL,
 title TEXT NOT NULL,
 detail TEXT NOT NULL,
 severity TEXT NOT NULL,
 disposition TEXT NOT NULL,
 decision_reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 decided_at TEXT NOT NULL,
 fix_sha TEXT NOT NULL,
 proof_note TEXT NOT NULL,
 resolved_at TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, pull_number, run_id, source_finding_id),
 UNIQUE (project_id, snapshot_id, pull_number, run_id, position),
 FOREIGN KEY (project_id, snapshot_id, pull_number, run_id)
  REFERENCES review_runs(project_id, snapshot_id, pull_number, id) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_findings_run_position
 ON review_findings(project_id, snapshot_id, pull_number, run_id, position);`,
		`CREATE TABLE IF NOT EXISTS review_attempts (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 state TEXT NOT NULL,
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 duration_seconds INTEGER CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
 source_run_id TEXT NOT NULL,
 trigger_id TEXT NOT NULL,
 provider_review_id TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, source, source_attempt_id),
 UNIQUE (project_id, snapshot_id, pull_number, position),
 FOREIGN KEY (project_id, snapshot_id, pull_number)
  REFERENCES review_pulls(project_id, snapshot_id, number) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_attempts_snapshot_pull_position
 ON review_attempts(project_id, snapshot_id, pull_number, position);`,
		`CREATE TABLE IF NOT EXISTS review_retired_pulls (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 reason TEXT NOT NULL CHECK (reason IN ('superseded','defunct')),
 PRIMARY KEY (project_id, snapshot_id, number),
 UNIQUE (project_id, snapshot_id, position),
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id) ON DELETE CASCADE
);`,
	})
}
