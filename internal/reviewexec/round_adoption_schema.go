package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
)

// RoundAdoptionSchemaSQL is the canonical optional provenance domain. A row
// records configuration history, never captured native completion credit.
func RoundAdoptionSchemaSQL() []string {
	return []string{`CREATE TABLE review_round_adoptions(
 project_id TEXT NOT NULL REFERENCES projects(id),pull_number INTEGER NOT NULL,channel TEXT NOT NULL,grant_revision INTEGER NOT NULL CHECK(grant_revision=1),epoch INTEGER NOT NULL CHECK(epoch=1),
 source TEXT NOT NULL,source_key TEXT NOT NULL,source_sha256 TEXT NOT NULL,prepared_json TEXT NOT NULL,
 PRIMARY KEY(project_id,pull_number,channel),UNIQUE(project_id,source,source_key),
 FOREIGN KEY(project_id,pull_number,channel,grant_revision,epoch) REFERENCES review_round_changes(project_id,pull_number,channel,revision,epoch));`}
}
func ensureRoundAdoptionSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_round_adoption", RoundAdoptionSchemaSQL())
}
