package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

// EnsureCheckpointSchema installs the private, immutable publication journal.
func EnsureCheckpointSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_checkpoint_publication", []string{
		`CREATE TABLE review_checkpoint_publications(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),evidence_id TEXT NOT NULL REFERENCES review_execution_evidence(id),state TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),created_at TEXT NOT NULL,body_json TEXT NOT NULL,UNIQUE(project_id,evidence_id))`,
		`CREATE INDEX review_checkpoint_pending ON review_checkpoint_publications(state,created_at,id)`,
		`CREATE INDEX review_checkpoint_project_pending ON review_checkpoint_publications(project_id,state)`,
		`CREATE TABLE review_checkpoint_revisions(publication_id TEXT NOT NULL REFERENCES review_checkpoint_publications(id),revision INTEGER NOT NULL,actor_id TEXT NOT NULL,created_at TEXT NOT NULL,body_json TEXT NOT NULL,PRIMARY KEY(publication_id,revision))`,
		`CREATE TABLE review_checkpoint_intents(publication_id TEXT NOT NULL REFERENCES review_checkpoint_publications(id),revision INTEGER NOT NULL,body_json TEXT NOT NULL,PRIMARY KEY(publication_id,revision))`,
	})
}
