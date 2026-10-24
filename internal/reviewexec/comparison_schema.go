package reviewexec

import (
	"context"
	"database/sql"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// ComparisonSchemaSQL owns the native-only derived proof ledger. Invalidation
// never grants credit: supported mutations revalidate their source before commit.
func ComparisonSchemaSQL() []string {
	statements := []string{
		`CREATE TABLE review_comparison_proofs (
 operation_id TEXT NOT NULL PRIMARY KEY REFERENCES review_operations(id),
 project_id TEXT NOT NULL REFERENCES projects(id),
 pull_number INTEGER NOT NULL CHECK(pull_number>0),
 channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),
 state TEXT NOT NULL CHECK(state IN ('dirty','pending','ready')),
 repository_id INTEGER NOT NULL DEFAULT 0 CHECK(repository_id>=0),
 candidate_sha TEXT NOT NULL DEFAULT '',
 base_sha TEXT NOT NULL DEFAULT '',
 parent_sha TEXT NOT NULL DEFAULT '',
 parent_pull_number INTEGER NOT NULL DEFAULT 0 CHECK(parent_pull_number>=0),
 base_ref TEXT NOT NULL DEFAULT '',
 evidence_id TEXT NOT NULL DEFAULT ''
);`,
		`CREATE INDEX review_comparison_proofs_state ON review_comparison_proofs(project_id,pull_number,channel,state,operation_id);`,
		`CREATE INDEX review_comparison_proofs_anchor ON review_comparison_proofs(project_id,pull_number,channel,state,repository_id,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,operation_id);`,
		`CREATE TRIGGER review_comparison_proofs_retain BEFORE DELETE ON review_comparison_proofs BEGIN SELECT RAISE(ABORT,'retain native comparison proof'); END;`,
	}
	// Both old and new relationships matter when inconsistent authority is
	// retagged directly. Queries start from completed native operations, including
	// completed operations that still have no capture. Imported rows cannot seed it.
	dependencies := []struct{ table, selectIDs string }{
		{"review_operations", `SELECT %s.id`},
		{"review_execution_attempts", `SELECT %s.operation_id`},
		{"review_execution_evidence", `SELECT %s.operation_id`},
		{"review_adjudication_state", `SELECT operation_id FROM review_execution_evidence WHERE id=%s.evidence_id`},
		{"review_finding_routes", `SELECT operation_id FROM review_execution_evidence WHERE id=%s.source_evidence_id`},
		{"review_finding_route_revisions", `SELECT e.operation_id FROM review_finding_routes r JOIN review_execution_evidence e ON e.id=r.source_evidence_id WHERE r.id=%s.route_id`},
	}
	for _, dependency := range dependencies {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			var ids []string
			if event != "INSERT" {
				ids = append(ids, strings.ReplaceAll(dependency.selectIDs, "%s", "OLD"))
			}
			if event != "DELETE" {
				ids = append(ids, strings.ReplaceAll(dependency.selectIDs, "%s", "NEW"))
			}
			query := strings.Join(ids, " UNION ")
			statements = append(statements, `CREATE TRIGGER review_comparison_dirty_`+dependency.table+`_`+strings.ToLower(event)+` AFTER `+event+` ON `+dependency.table+` BEGIN
 INSERT INTO review_comparison_proofs(operation_id,project_id,pull_number,channel,state)
 SELECT o.id,o.project_id,o.pull_number,o.channel,'dirty' FROM review_operations o
 WHERE o.id IN (`+query+`) AND (o.state='completed' OR EXISTS(SELECT 1 FROM review_comparison_proofs p WHERE p.operation_id=o.id))
 ON CONFLICT(operation_id) DO UPDATE SET project_id=excluded.project_id,pull_number=excluded.pull_number,channel=excluded.channel,state='dirty'; END;`)
		}
	}
	statements = append(statements, `CREATE TRIGGER review_comparison_repository_changed AFTER UPDATE OF github_repository_id ON projects WHEN OLD.github_repository_id IS NOT NEW.github_repository_id BEGIN UPDATE review_comparison_proofs SET state='dirty' WHERE project_id=OLD.id OR project_id=NEW.id; END;`)
	return statements
}

func ensureComparisonSchema(ctx context.Context, store *core.Store) error {
	if err := store.EnsureSchemaWithBackfill(ctx, "review_comparison", [][]string{ComparisonSchemaSQL()}, backfillComparisonProofs); err != nil {
		return err
	}
	return store.EnsureSchema(ctx, "review_applicability", ApplicabilitySchemaSQL())
}

func backfillComparisonProofs(ctx context.Context, conn *sql.Conn) error {
	last := ""
	for {
		rows, err := conn.QueryContext(ctx, `SELECT id,project_id FROM review_operations WHERE state='completed' AND id>? ORDER BY id LIMIT 128`, last)
		if err != nil {
			return err
		}
		type selected struct{ id, project string }
		var page []selected
		for rows.Next() {
			var row selected
			if err = rows.Scan(&row.id, &row.project); err != nil {
				break
			}
			page = append(page, row)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range page {
			if err := refreshComparisonProof(ctx, conn, row.project, row.id); err != nil {
				return err
			}
			last = row.id
		}
		if len(page) < 128 {
			break
		}
	}
	// Each inserted proof was already derived from retained authority above.
	// Under the exclusive migration transaction, only the set check remains;
	// backup validation still independently re-derives every retained proof.
	return validateComparisonProofSet(ctx, conn)
}
