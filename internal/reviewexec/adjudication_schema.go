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
	})
}
