package jobs

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureSchema installs the jobs tables during exclusive startup, before the
// API begins serving requests. Runtime handlers never perform schema changes.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "jobs", []string{
		`CREATE TABLE IF NOT EXISTS jobs (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 title TEXT NOT NULL,
 brief TEXT NOT NULL,
 worker_id TEXT,
 status TEXT NOT NULL CHECK (status IN ('active', 'parked', 'blocked', 'completed')),
 revision INTEGER NOT NULL CHECK (revision >= 1),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 PRIMARY KEY (project_id, id),
 FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
 FOREIGN KEY (project_id, worker_id) REFERENCES workers(project_id, id)
);`,
		`CREATE INDEX IF NOT EXISTS jobs_project_order ON jobs(project_id, id);`,
		`CREATE INDEX IF NOT EXISTS jobs_project_worker_order ON jobs(project_id, worker_id, id);`,
		`CREATE INDEX IF NOT EXISTS jobs_project_status_order ON jobs(project_id, status, id);`,
		`CREATE TABLE IF NOT EXISTS job_history (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 actor_id TEXT NOT NULL,
 state_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id, job_id, revision),
 FOREIGN KEY (project_id, job_id) REFERENCES jobs(project_id, id) ON DELETE CASCADE
);`,
	}, []string{
		`CREATE TABLE IF NOT EXISTS job_checkpoints (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 done TEXT NOT NULL,
 next TEXT NOT NULL,
 blocker TEXT NOT NULL,
 evidence_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 FOREIGN KEY (project_id, job_id) REFERENCES jobs(project_id, id) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS job_checkpoints_project_job_sequence ON job_checkpoints(project_id, job_id, sequence DESC);`,
	}, []string{
		`CREATE TABLE job_updates (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 body TEXT NOT NULL,
 evidence_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 FOREIGN KEY (project_id, job_id) REFERENCES jobs(project_id, id) ON DELETE CASCADE
);`,
		`CREATE INDEX job_updates_project_job_sequence ON job_updates(project_id,job_id,sequence DESC);`,
		`CREATE TABLE job_checklists (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 items_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id,job_id),
 FOREIGN KEY (project_id, job_id) REFERENCES jobs(project_id, id) ON DELETE CASCADE
);`,
		`CREATE TABLE job_checklist_history (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 items_json TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id,job_id,revision),
 FOREIGN KEY (project_id, job_id) REFERENCES jobs(project_id, id) ON DELETE CASCADE
);`,
	}, []string{
		`CREATE TABLE worker_primary_jobs (
 project_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_id TEXT,
 PRIMARY KEY (project_id,worker_id),
 FOREIGN KEY (project_id,worker_id) REFERENCES workers(project_id,id),
 FOREIGN KEY (project_id,job_id) REFERENCES jobs(project_id,id)
);`,
		`CREATE TABLE worker_primary_job_history (
 project_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_id TEXT,
 actor_id TEXT NOT NULL,
 actor_worker_id TEXT NOT NULL,
 reason TEXT NOT NULL CHECK (reason IN ('selected','cleared','job_reassigned')),
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id,worker_id,revision),
 FOREIGN KEY (project_id,worker_id) REFERENCES workers(project_id,id),
 FOREIGN KEY (project_id,job_id) REFERENCES jobs(project_id,id)
);`,
	})
}
