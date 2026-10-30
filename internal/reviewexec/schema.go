package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSchema(ctx context.Context, store *core.Store) error {
	if err := adoptionholds.EnsureSchema(ctx, store); err != nil {
		return err
	}
	if err := EnsurePolicySchema(ctx, store); err != nil {
		return err
	}
	if err := store.EnsureSchemaWithBackfill(ctx, "review_execution", [][]string{{
		`CREATE TABLE review_execution_config(project_id TEXT NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),body_json TEXT NOT NULL,PRIMARY KEY(project_id,revision))`,
		`CREATE TABLE review_operations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,configuration_revision INTEGER NOT NULL,pull_number INTEGER NOT NULL,channel TEXT NOT NULL,head_sha TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,reason TEXT NOT NULL,attempt_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision))`,
		`CREATE INDEX review_operations_pending ON review_operations(state,created_at,id)`,
		`CREATE TABLE review_execution_attempts(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL UNIQUE REFERENCES review_operations(id),project_id TEXT NOT NULL REFERENCES projects(id),provider TEXT NOT NULL,account TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,anchor_json TEXT NOT NULL,intent TEXT NOT NULL,external_id TEXT NOT NULL)`,
		`CREATE TABLE review_execution_reservations(provider TEXT NOT NULL,repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,action_class TEXT NOT NULL,account TEXT NOT NULL,attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),PRIMARY KEY(provider,repository_id,pull_number,action_class),UNIQUE(provider,account,action_class))`,
		`CREATE TABLE review_execution_transitions(attempt_id TEXT NOT NULL REFERENCES review_execution_attempts(id),revision INTEGER NOT NULL,state TEXT NOT NULL,reason TEXT NOT NULL,external_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(attempt_id,revision))`,
	}, {`ALTER TABLE review_operations ADD COLUMN capability_fingerprint TEXT NOT NULL DEFAULT ''`}, {
		`ALTER TABLE review_operations ADD COLUMN policy_revision INTEGER NOT NULL DEFAULT 0`,
	}, {
		`CREATE TABLE review_cli_preflight(attempt_id TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),project_id TEXT NOT NULL REFERENCES projects(id),body_json TEXT NOT NULL)`,
		`CREATE INDEX review_operations_cli_admission ON review_operations(project_id,pull_number,channel,state,id)`,
	}, {
		`CREATE INDEX review_operations_completed_comparison ON review_operations(project_id,channel,pull_number,id) WHERE state='completed'`,
		`DROP INDEX review_operations_cli_admission`,
	}, {
		`CREATE TABLE review_hosted_preflight(attempt_id TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),project_id TEXT NOT NULL REFERENCES projects(id),body_json TEXT NOT NULL)`,
		`CREATE TABLE review_hosted_allowance_queries(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),capability_fingerprint TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision IN (1,2)),body_json TEXT NOT NULL)`,
	}, {
		`ALTER TABLE review_operations ADD COLUMN allow_out_of_order INTEGER NOT NULL DEFAULT 0 CHECK(allow_out_of_order IN (0,1))`,
	}, {
		`ALTER TABLE review_operations ADD COLUMN override_file_limit INTEGER NOT NULL DEFAULT 0 CHECK(override_file_limit IN (0,1))`,
	}, {
		`PRAGMA defer_foreign_keys=ON`,
		`CREATE TABLE review_execution_attempts_next(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL REFERENCES review_operations(id),project_id TEXT NOT NULL REFERENCES projects(id),provider TEXT NOT NULL,account TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,anchor_json TEXT NOT NULL,intent TEXT NOT NULL,external_id TEXT NOT NULL)`,
		`INSERT INTO review_execution_attempts_next SELECT id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts`,
		`DROP TABLE review_execution_attempts`,
		`ALTER TABLE review_execution_attempts_next RENAME TO review_execution_attempts`,
		`CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id)`,
		`CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id) WHERE state IN ('preparing','posting','active','uncertain')`,
		`CREATE TABLE review_operation_waits(operation_id TEXT PRIMARY KEY REFERENCES review_operations(id),baseline_attempt_id TEXT REFERENCES review_execution_attempts(id),retry_not_before TEXT NOT NULL DEFAULT '',paused INTEGER NOT NULL DEFAULT 0 CHECK(paused IN(0,1)),CHECK((baseline_attempt_id IS NULL AND retry_not_before='') OR (baseline_attempt_id IS NOT NULL AND retry_not_before<>'')))`,
	}}, validateExecutionForeignKeys); err != nil {
		return err
	}
	if err := store.EnsureSchema(ctx, "review_source_refusals", []string{`ALTER TABLE review_execution_transitions ADD COLUMN source_refusal_json TEXT NOT NULL DEFAULT 'null' CHECK(length(CAST(source_refusal_json AS BLOB))<=512)`}, []string{`SELECT 1 /* source-refusal format: unavailable parent facts */`}, []string{`SELECT 1 /* source-refusal format: unfinished predecessor cause */`}); err != nil {
		return err
	}
	// Index maintenance has its own version so newer execution-table
	// migrations retain their existing positions in deployed databases.
	if err := store.EnsureSchema(ctx, "review_execution_indexes", []string{`DROP INDEX IF EXISTS review_operations_coverage`}); err != nil {
		return err
	}
	if err := EnsureEvidenceSchema(ctx, store); err != nil {
		return err
	}
	if err := EnsureAdjudicationSchema(ctx, store); err != nil {
		return err
	}
	if err := EnsureSourceRefreshSchema(ctx, store); err != nil {
		return err
	}
	if err := EnsureCheckpointSchema(ctx, store); err != nil {
		return err
	}
	if err := ensureComparisonSchema(ctx, store); err != nil {
		return err
	}
	if err := ensureRoutePolicySchema(ctx, store); err != nil {
		return err
	}
	if err := EnsureRoundSchema(ctx, store); err != nil {
		return err
	}
	if err := ensureRoundAdoptionSchema(ctx, store); err != nil {
		return err
	}
	if err := EnsureSelectionSchema(ctx, store); err != nil {
		return err
	}
	return EnsureFrontSchema(ctx, store)
}

// Reconstruction keeps every dependent row and verifies all constraints before
// the exclusive migration transaction commits. No foreign key uses cascade.
func validateExecutionForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	invalid := rows.Next()
	rowErr := rows.Err()
	closeErr := rows.Close()
	if invalid {
		return core.Fail("schema_invalid", "Execution migration found inconsistent retained references.", 500)
	}
	if rowErr != nil {
		return rowErr
	}
	if closeErr != nil {
		return closeErr
	}
	var enabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return err
	}
	if enabled != 1 {
		return core.Fail("schema_invalid", "Execution migration requires enforced retained references.", 500)
	}
	// SQLite retains deferred violation bookkeeping from DROP of the original
	// parent even when its replacement satisfies every child reference. Clear
	// that bookkeeping only after checking all real constraints. Enforcement
	// remains enabled throughout the exclusive migration and at commit.
	_, err = conn.ExecContext(ctx, `PRAGMA defer_foreign_keys=OFF`)
	return err
}
