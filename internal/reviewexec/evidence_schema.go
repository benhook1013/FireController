package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureEvidenceSchema installs the private immutable capture ledger at startup.
func EnsureEvidenceSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_evidence", []string{
		`CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),body_json TEXT NOT NULL,fingerprint TEXT NOT NULL)`,
	}, []string{`CREATE INDEX review_evidence_operation ON review_execution_evidence(project_id,operation_id,id)`})
}
