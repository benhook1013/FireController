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
				for _, index := range []string{"review_waits_live_retry", "review_transitions_exhaustion_time", "review_transitions_exhaustion_attempt"} {
					admissionSQL(t, f, "DROP INDEX "+index)
				}
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
			if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution_indexes'`).Scan(&version); err != nil || version != "3" {
				t.Fatal("index migration missing", version, err)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("repeat startup", err)
			}
		})
	}
}

func TestPendingOperationListDoesNotTraverseTerminalHistory(t *testing.T) {
	f := operationListFixture(t)
	live := seedOperationListRow(t, f, 1, attemptProject, "active", "cli", 2)
	// Terminal and unreserved-held rows are newer than the one live operation.
	// An ordered history scan must visit every one before finding that operation.
	for _, retained := range []int{1000, 5000} {
		admissionSQL(t, f, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT OR IGNORE INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at) SELECT printf('review_operation_%032x',100+x),?,'private-actor','worker-list',1,2,'cli',?,CASE WHEN x%2=0 THEN 'completed' ELSE 'held' END,1,'','','2026-10-24T12:00:00Z','2026-10-24T12:00:00Z' FROM n`, retained, attemptProject, strings.Repeat("a", 40))
		for _, continuation := range []bool{false, true} {
			query := `EXPLAIN QUERY PLAN SELECT o.id,o.pull_number,o.channel,o.head_sha,o.worker_id,o.state,o.reason,o.attempt_id,o.revision,o.created_at,o.updated_at FROM ` + pendingOperationCandidates + ` WHERE o.project_id=?`
			args := []any{attemptProject, attemptProject, attemptProject}
			if continuation {
				query += ` AND (o.created_at,o.id)<(?,?)`
				args = append(args, "2026-10-25T12:00:00Z", "review_operation_ffffffffffffffffffffffffffffffff")
			}
			query += ` ORDER BY o.created_at DESC,o.id DESC LIMIT 6`
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
			if err != nil || !strings.Contains(plan, "review_operations_pending (state=?)") || !strings.Contains(plan, "SCAN r") || strings.Contains(plan, "review_operations_listing") || strings.Contains(plan, "SCAN o") || strings.Contains(plan, "SCAN p\n") {
				t.Fatal("pending query traverses retained history", retained, continuation, plan, err)
			}
			t.Logf("retained=%d continuation=%t plan:\n%s", retained, continuation, plan)
		}
		for _, filter := range []string{"?channel=hosted", "?pull_number=3"} {
			if page := listOperationPage(t, f, filter); len(page.Items) != 0 || page.HasMore {
				t.Fatal("pending filters changed", filter, page)
			}
		}
		attention := listOperationPage(t, f, "?status=attention&limit=1")
		if len(attention.Items) != 1 || attention.Items[0].ID != live || attention.HasMore {
			t.Fatal("pending optimization changed attention selection", retained, attention)
		}
		page := listOperationPage(t, f, "")
		if len(page.Items) != 1 || page.Items[0].ID != live || page.HasMore {
			t.Fatal("pending history changed result", retained, page)
		}
	}
}
