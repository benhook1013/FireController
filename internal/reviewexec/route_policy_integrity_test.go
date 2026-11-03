package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestRoutePolicyInvalidationRetainsEveryOldAndNewScope(t *testing.T) {
	f, _, evidence := routeFixture(t, 1)
	created := routeCreate(t, f, evidence, "scope-route", routeRequest(0, 0, 4))
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('route-other-project','Other',992,'synthetic/other')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE review_finding_routes SET target_pull_number=6 WHERE id=?`,
		`UPDATE review_finding_routes SET target_pull_number=8 WHERE id=?`,
		`UPDATE review_finding_routes SET project_id='route-other-project' WHERE id=?`,
	} {
		if _, err := f.store.DB().Exec(statement, created.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []struct {
		project string
		target  int64
	}{{attemptProject, 4}, {attemptProject, 6}, {attemptProject, 8}, {"route-other-project", 8}} {
		err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := pendingIncomingRoutes(ctx, tx, scope.project, scope.target)
			return err
		})
		if err == nil {
			t.Fatal("raw retag lost an invalid receiving scope", scope)
		}
	}
	if err := ValidateRoutePolicyFacts(t.Context(), f.store.DB()); err == nil {
		t.Fatal("invalid scope graph accepted")
	}
}

func TestRoutePolicyRefreshingOneRoutePreservesAnotherInvalidation(t *testing.T) {
	f, _, evidence := routeFixture(t, 2)
	first := routeCreate(t, f, evidence, "dirty-first", routeRequest(0, 0, 4))
	second := routeCreate(t, f, evidence, "clean-second", routeRequest(1, 1, 4))
	routeDecision(t, f, first.ID, "first-resolved", 1, "rejected")
	if _, err := f.store.DB().Exec(`UPDATE review_finding_route_revisions SET body_json='{}' WHERE route_id=? AND revision=1`, first.ID); err != nil {
		t.Fatal(err)
	}
	routeDecision(t, f, second.ID, "second-resolved", 1, "rejected")
	if _, err := routePending(t, f, 4); err == nil {
		t.Fatal("another route mutation cleared invalid resolved history")
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_route_policy_invalidations WHERE route_id=?`, first.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal("dirty route marker lost", n, err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_route_policy_invalidations WHERE route_id=?`, second.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("valid route did not settle only itself", n, err)
	}
}

func TestRoutePolicyDerivedDeletionGuardAndFullValidation(t *testing.T) {
	for _, mode := range []string{"forged-resolved", "missing-fact", "stale-revision"} {
		t.Run(mode, func(t *testing.T) {
			f, _, evidence := routeFixture(t, 1)
			created := routeCreate(t, f, evidence, "fact-route", routeRequest(0, 0, 4))
			if _, err := f.store.DB().Exec(`DELETE FROM review_route_policy_facts WHERE route_id=?`, created.ID); err == nil {
				t.Fatal("derived fact independently deleted")
			}
			switch mode {
			case "forged-resolved":
				if _, err := f.store.DB().Exec(`UPDATE review_route_policy_facts SET pending=0 WHERE route_id=?`, created.ID); err != nil {
					t.Fatal(err)
				}
			case "missing-fact":
				if _, err := f.store.DB().Exec(`DROP TRIGGER review_route_policy_facts_retain; DELETE FROM review_route_policy_facts`); err != nil {
					t.Fatal(err)
				}
			case "stale-revision":
				if _, err := f.store.DB().Exec(`UPDATE review_route_policy_facts SET revision=revision+1 WHERE route_id=?`, created.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateRoutePolicyFacts(t.Context(), f.store.DB()); err == nil {
				t.Fatal("damaged derived graph accepted", mode)
			}
		})
	}
}

func TestRoutePolicyQueriesUseScopedIndexes(t *testing.T) {
	f, _, evidence := routeFixture(t, 1)
	created := routeCreate(t, f, evidence, "query-route", routeRequest(0, 0, 4))
	cases := []struct {
		query, index string
		args         []any
	}{
		{`SELECT count(*) FROM review_route_policy_facts WHERE project_id=? AND target_pull_number=? AND active=1 AND pending=1`, "review_route_policy_facts_pending", []any{attemptProject, int64(4)}},
		{`SELECT EXISTS(SELECT 1 FROM review_route_policy_invalidations WHERE project_id=? AND target_pull_number=?)`, "sqlite_autoindex_review_route_policy_invalidations_1", []any{attemptProject, int64(4)}},
		{`DELETE FROM review_route_policy_invalidations WHERE route_id=?`, "review_route_policy_invalidations_route", []any{created.ID}},
	}
	for _, tc := range cases {
		rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, tc.index) {
				found = true
			}
			if strings.Contains(detail, "SCAN review_route_policy") || strings.Contains(detail, "TEMP B-TREE") {
				rows.Close()
				t.Fatal("unbounded route query plan", detail)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatal("missing targeted route index", tc.index, err)
		}
	}
}
