package reviewexec

import (
	"strings"
	"testing"
)

func TestOperationListIndexFreshAndExistingDatabase(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "v1_upgrade"
		}
		t.Run(name, func(t *testing.T) {
			f, a, _ := cliAllowanceFixture(t)
			if upgrade {
				admissionSQL(t, f, `DROP INDEX IF EXISTS review_operations_listing`)
				admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_execution_indexes'`)
			}
			before := attemptState(t, f, a)
			receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			if before != attemptState(t, f, a) || receipts != attemptCount(t, f.store, `SELECT count(*) FROM receipts`) {
				t.Fatal("index upgrade changed retained execution")
			}
			for _, cursor := range []bool{false, true} {
				query := `EXPLAIN QUERY PLAN SELECT o.id,o.pull_number,o.channel,o.head_sha,o.worker_id,o.state,o.reason,o.attempt_id,o.revision,o.created_at,o.updated_at FROM review_operations o WHERE o.project_id=?`
				args := []any{attemptProject}
				if cursor {
					query += ` AND (o.created_at,o.id)<(?,?)`
					args = append(args, "2026-10-23T12:00:00Z", "review_operation_ffffffffffffffffffffffffffffffff")
				}
				query += ` ORDER BY o.created_at DESC,o.id DESC LIMIT ?`
				args = append(args, 6)
				rows, err := f.store.DB().Query(query, args...)
				if err != nil {
					t.Fatal(err)
				}
				plan := ""
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					plan += detail + "\n"
				}
				err = rows.Err()
				rows.Close()
				if err != nil || !strings.Contains(plan, "review_operations_listing") || strings.Contains(plan, "TEMP B-TREE") {
					t.Fatal("page reads lack ordered index", plan, err)
				}
			}
			var version string
			if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution_indexes'`).Scan(&version); err != nil || version != "2" {
				t.Fatal("index migration missing", version, err)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("repeat startup", err)
			}
		})
	}
}
