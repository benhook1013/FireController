package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureAdjudicationSchema installs the disposition ledger at startup.
func EnsureAdjudicationSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_adjudication", []string{
		`CREATE TABLE review_adjudication_state(evidence_id TEXT PRIMARY KEY REFERENCES review_execution_evidence(id),project_id TEXT NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),body_json TEXT NOT NULL)`,
		`CREATE TABLE review_adjudication_changes(evidence_id TEXT NOT NULL REFERENCES review_execution_evidence(id),revision INTEGER NOT NULL CHECK(revision>0),project_id TEXT NOT NULL REFERENCES projects(id),actor_id TEXT NOT NULL,kind TEXT NOT NULL,finding_index INTEGER NOT NULL,input_json TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(evidence_id,revision))`,
	}, []string{
		`CREATE TABLE review_finding_routes(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),repository_id INTEGER NOT NULL,source_evidence_id TEXT NOT NULL REFERENCES review_execution_evidence(id),finding_index INTEGER NOT NULL,source_pull_number INTEGER NOT NULL,source_head_sha TEXT NOT NULL,target_pull_number INTEGER NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),disposition TEXT NOT NULL,has_fix INTEGER NOT NULL CHECK(has_fix IN (0,1)),created_at TEXT NOT NULL,body_json TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX review_finding_routes_source_active ON review_finding_routes(project_id,source_evidence_id,finding_index) WHERE state='active'`,
		`CREATE INDEX review_finding_routes_incoming ON review_finding_routes(project_id,target_pull_number,state,created_at DESC,id DESC)`,
		`CREATE TABLE review_finding_route_revisions(route_id TEXT NOT NULL REFERENCES review_finding_routes(id),project_id TEXT NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),actor_id TEXT NOT NULL,kind TEXT NOT NULL,created_at TEXT NOT NULL,body_json TEXT NOT NULL,PRIMARY KEY(route_id,revision))`,
	})
}
