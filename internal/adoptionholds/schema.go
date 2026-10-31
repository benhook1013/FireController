// Package adoptionholds retains source obligations without native review credit.
package adoptionholds

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

func SchemaSQL() []string {
	return []string{
		`CREATE TABLE review_adoption_sources(project_id TEXT NOT NULL REFERENCES projects(id),namespace TEXT NOT NULL,repository_id INTEGER NOT NULL,snapshot_sha256 TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),PRIMARY KEY(project_id,namespace));`,
		`CREATE TABLE review_adoption_imports(project_id TEXT NOT NULL,namespace TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),batch_json TEXT NOT NULL,batch_sha256 TEXT NOT NULL,actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,namespace,revision),FOREIGN KEY(project_id,namespace) REFERENCES review_adoption_sources(project_id,namespace));`,
		`CREATE TABLE review_adoption_obligations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,namespace TEXT NOT NULL,source_key TEXT NOT NULL,source_revision INTEGER NOT NULL,original_json TEXT NOT NULL,original_sha256 TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('open','resolved')),revision INTEGER NOT NULL CHECK(revision IN (1,2)),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(project_id,namespace,source_key),UNIQUE(id,project_id),FOREIGN KEY(project_id,namespace,source_revision) REFERENCES review_adoption_imports(project_id,namespace,revision));`,
		`CREATE TABLE review_adoption_targets(obligation_id TEXT NOT NULL,project_id TEXT NOT NULL,pull_number INTEGER NOT NULL CHECK(pull_number BETWEEN 1 AND 2147483647),PRIMARY KEY(obligation_id,pull_number),FOREIGN KEY(obligation_id,project_id) REFERENCES review_adoption_obligations(id,project_id));`,
		`CREATE INDEX review_adoption_targets_scope ON review_adoption_targets(project_id,pull_number,obligation_id);`,
		`CREATE INDEX review_adoption_obligations_list ON review_adoption_obligations(project_id,status,id);`,
		`CREATE TABLE review_adoption_resolutions(obligation_id TEXT PRIMARY KEY REFERENCES review_adoption_obligations(id),revision INTEGER NOT NULL CHECK(revision=2),reason TEXT NOT NULL,evidence_json TEXT NOT NULL,actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,resolved_at TEXT NOT NULL);`,
		`CREATE TRIGGER review_adoption_targets_retain BEFORE DELETE ON review_adoption_targets BEGIN SELECT RAISE(ABORT,'retain obligation scope'); END;`,
		`CREATE TRIGGER review_adoption_targets_immutable BEFORE UPDATE ON review_adoption_targets BEGIN SELECT RAISE(ABORT,'immutable obligation scope'); END;`,
		`CREATE TRIGGER review_adoption_obligations_retain BEFORE DELETE ON review_adoption_obligations BEGIN SELECT RAISE(ABORT,'retain source obligation'); END;`,
		`CREATE TRIGGER review_adoption_originals_immutable BEFORE UPDATE OF id,project_id,namespace,source_key,source_revision,original_json,original_sha256,actor_id,worker_id,created_at ON review_adoption_obligations BEGIN SELECT RAISE(ABORT,'immutable source obligation'); END;`,
		`CREATE TRIGGER review_adoption_imports_retain BEFORE DELETE ON review_adoption_imports BEGIN SELECT RAISE(ABORT,'retain source import'); END;`,
		`CREATE TRIGGER review_adoption_imports_immutable BEFORE UPDATE ON review_adoption_imports BEGIN SELECT RAISE(ABORT,'immutable source import'); END;`,
		`CREATE TRIGGER review_adoption_resolutions_retain BEFORE DELETE ON review_adoption_resolutions BEGIN SELECT RAISE(ABORT,'retain obligation resolution'); END;`,
		`CREATE TRIGGER review_adoption_resolutions_immutable BEFORE UPDATE ON review_adoption_resolutions BEGIN SELECT RAISE(ABORT,'immutable obligation resolution'); END;`,
	}
}
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_adoption_obligations", SchemaSQL())
}
