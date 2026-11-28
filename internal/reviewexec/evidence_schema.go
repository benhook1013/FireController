package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureEvidenceSchema installs the private immutable capture ledger at startup.
func EnsureEvidenceSchema(ctx context.Context, store *core.Store) error {
	if err := store.EnsureSchema(ctx, "review_evidence", []string{
		`CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),body_json TEXT NOT NULL,fingerprint TEXT NOT NULL)`,
	}, []string{`CREATE INDEX review_evidence_operation ON review_execution_evidence(project_id,operation_id,id)`}, []string{
		`CREATE TABLE review_hosted_observations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),position INTEGER NOT NULL CHECK(position BETWEEN 1 AND 200),capture_sha256 TEXT NOT NULL,capture_bytes INTEGER NOT NULL CHECK(capture_bytes BETWEEN 1 AND 50331648),body_json TEXT NOT NULL,UNIQUE(attempt_id,capture_sha256),UNIQUE(attempt_id,position))`,
		`CREATE INDEX review_hosted_observation_attempt ON review_hosted_observations(project_id,attempt_id,position)`,
	}, ExternalHostedEvidenceIndexSQL(), []string{
		`CREATE TABLE review_activity_evidence(attempt_id TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),project_id TEXT NOT NULL REFERENCES projects(id),body_json TEXT NOT NULL CHECK(length(CAST(body_json AS BLOB))<=2048))`,
	}, CLIEvidenceRecoverySchemaSQL()); err != nil {
		return err
	}
	return store.EnsureSchema(ctx, "review_hosted_submission_evidence", []string{HostedSubmissionEvidenceTableSQL})
}

const HostedSubmissionEvidenceTableSQL = `CREATE TABLE review_hosted_submission_evidence(attempt_id TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),project_id TEXT NOT NULL REFERENCES projects(id),head_sha TEXT NOT NULL CHECK(length(head_sha)=40),trigger_id TEXT NOT NULL CHECK(length(trigger_id) BETWEEN 1 AND 19),submitted_at TEXT NOT NULL CHECK(length(submitted_at) BETWEEN 20 AND 30),observed_at TEXT NOT NULL CHECK(length(observed_at) BETWEEN 20 AND 30))`

// ExternalHostedEvidenceIndexSQL supports passive exact-identity lookup without
// scanning a project or imposing a lifetime native-review count ceiling.
func ExternalHostedEvidenceIndexSQL() []string {
	return []string{
		`CREATE INDEX review_evidence_hosted_publication ON review_execution_evidence(project_id,json_extract(body_json,'$.capture.hosted.publication_id'));`,
		`CREATE INDEX review_evidence_hosted_run ON review_execution_evidence(project_id,coalesce(json_extract(body_json,'$.capture.hosted.submitted_review.provider_run_id'),json_extract(body_json,'$.capture.hosted.standing_summary.provider_run_id')));`,
	}
}

// CLIEvidenceRecoverySchemaSQL is the canonical immutable interpretation archive.
func CLIEvidenceRecoverySchemaSQL() []string {
	return []string{`CREATE TABLE review_cli_evidence_recovery(evidence_id TEXT PRIMARY KEY REFERENCES review_execution_evidence(id),project_id TEXT NOT NULL REFERENCES projects(id),original_body_json TEXT NOT NULL,original_fingerprint TEXT NOT NULL,recovered_fingerprint TEXT NOT NULL,recovered_at TEXT NOT NULL)`,
		`CREATE TRIGGER review_cli_recovery_retain BEFORE DELETE ON review_cli_evidence_recovery BEGIN SELECT RAISE(ABORT,'retain CLI recovery evidence'); END`,
		`CREATE TRIGGER review_cli_recovery_immutable BEFORE UPDATE ON review_cli_evidence_recovery BEGIN SELECT RAISE(ABORT,'retain CLI recovery evidence'); END`}
}
