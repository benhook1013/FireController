package reviewexec

import (
	"context"
	"database/sql"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// RoutePolicySchemaSQL owns derived receiving-scope facts, not source review
// coverage. Invalidations retain every affected scope until that route validates.
func RoutePolicySchemaSQL() []string {
	statements := []string{
		`CREATE TABLE review_route_policy_facts (
 route_id TEXT NOT NULL PRIMARY KEY REFERENCES review_finding_routes(id),
 project_id TEXT NOT NULL REFERENCES projects(id),
 target_pull_number INTEGER NOT NULL CHECK(target_pull_number>0),
 revision INTEGER NOT NULL CHECK(revision>0),
 active INTEGER NOT NULL CHECK(active IN (0,1)),
 pending INTEGER NOT NULL CHECK(pending IN (0,1) AND pending<=active)
);`,
		`CREATE INDEX review_route_policy_facts_pending ON review_route_policy_facts(project_id,target_pull_number,active,pending,route_id);`,
		`CREATE TABLE review_route_policy_invalidations (
 project_id TEXT NOT NULL REFERENCES projects(id),
 target_pull_number INTEGER NOT NULL CHECK(target_pull_number>0),
 route_id TEXT NOT NULL,
 PRIMARY KEY(project_id,target_pull_number,route_id)
);`,
		`CREATE INDEX review_route_policy_invalidations_route ON review_route_policy_invalidations(route_id);`,
		`CREATE TRIGGER review_route_policy_facts_retain BEFORE DELETE ON review_route_policy_facts BEGIN SELECT RAISE(ABORT,'retain receiving route policy fact'); END;`,
	}
	for _, table := range []string{"review_finding_routes", "review_finding_route_revisions"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			var selects []string
			for _, row := range []string{"OLD", "NEW"} {
				if event == "INSERT" && row == "OLD" || event == "DELETE" && row == "NEW" {
					continue
				}
				if table == "review_finding_routes" {
					selects = append(selects, `SELECT `+row+`.project_id,`+row+`.target_pull_number,`+row+`.id`)
				} else {
					selects = append(selects, `SELECT project_id,target_pull_number,id FROM review_finding_routes WHERE id=`+row+`.route_id`)
					selects = append(selects, `SELECT project_id,target_pull_number,route_id FROM review_route_policy_facts WHERE route_id=`+row+`.route_id`)
				}
			}
			statements = append(statements, `CREATE TRIGGER review_route_policy_dirty_`+table+`_`+strings.ToLower(event)+` AFTER `+event+` ON `+table+` BEGIN
 INSERT OR IGNORE INTO review_route_policy_invalidations(project_id,target_pull_number,route_id) `+strings.Join(selects, " UNION ")+`; END;`)
		}
	}
	return statements
}

func ensureRoutePolicySchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchemaWithBackfill(ctx, "review_route_policy", [][]string{RoutePolicySchemaSQL()}, backfillRoutePolicyFacts)
}

func backfillRoutePolicyFacts(ctx context.Context, conn *sql.Conn) error {
	var malformed bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_finding_routes WHERE id IS NULL OR id='')`).Scan(&malformed); err != nil {
		return err
	}
	if malformed {
		return routeStorageError()
	}
	last := ""
	for {
		rows, err := conn.QueryContext(ctx, `SELECT id,project_id FROM review_finding_routes WHERE id>? ORDER BY id LIMIT 128`, last)
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
			if err := refreshRoutePolicyFact(ctx, conn, row.project, row.id); err != nil {
				return err
			}
			last = row.id
		}
		if len(page) < 128 {
			break
		}
	}
	return ValidateRoutePolicyFacts(ctx, conn)
}
