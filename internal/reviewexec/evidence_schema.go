package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureEvidenceSchema installs the private immutable capture ledger at startup.
func EnsureEvidenceSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_evidence", []string{
		`CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),body_json TEXT NOT NULL,fingerprint TEXT NOT NULL)`,
	}, []string{`CREATE INDEX review_evidence_operation ON review_execution_evidence(project_id,operation_id,id)`}, []string{
		`CREATE TABLE review_hosted_observations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),position INTEGER NOT NULL CHECK(position BETWEEN 1 AND 200),capture_sha256 TEXT NOT NULL,capture_bytes INTEGER NOT NULL CHECK(capture_bytes BETWEEN 1 AND 50331648),body_json TEXT NOT NULL,UNIQUE(attempt_id,capture_sha256),UNIQUE(attempt_id,position))`,
		`CREATE INDEX review_hosted_observation_attempt ON review_hosted_observations(project_id,attempt_id,position)`,
	}, ExternalHostedEvidenceIndexSQL())
}

// ExternalHostedEvidenceIndexSQL supports passive exact-identity lookup without
// scanning a project or imposing a lifetime native-review count ceiling.
func ExternalHostedEvidenceIndexSQL() []string {
	return []string{
		`CREATE INDEX review_evidence_hosted_publication ON review_execution_evidence(project_id,json_extract(body_json,'$.capture.hosted.publication_id'));`,
		`CREATE INDEX review_evidence_hosted_run ON review_execution_evidence(project_id,coalesce(json_extract(body_json,'$.capture.hosted.submitted_review.provider_run_id'),json_extract(body_json,'$.capture.hosted.standing_summary.provider_run_id')));`,
	}
}
