package reviewexec

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Remove only this additive domain, retaining the predecessor's exact route,
// journal, evidence and source schemas rather than relabelling their versions.
func routePolicyPreDomain(t *testing.T, f *attemptFixture) {
	t.Helper()
	objects := map[string][]string{}
	for _, statement := range RoutePolicySchemaSQL() {
		words := strings.Fields(statement)
		if len(words) < 3 || words[0] != "CREATE" {
			t.Fatal("unexpected route-policy DDL")
		}
		objects[words[1]] = append(objects[words[1]], words[2])
	}
	for _, kind := range []string{"TRIGGER", "INDEX", "TABLE"} {
		for _, name := range objects[kind] {
			if _, err := f.store.DB().Exec("DROP " + kind + " " + name); err != nil {
				t.Fatal(kind, name, err)
			}
		}
	}
	if _, err := f.store.DB().Exec(`DELETE FROM metadata WHERE key='schema:review_route_policy'`); err != nil {
		t.Fatal(err)
	}
	assertRoutePolicyDomainAbsent(t, f)
}

func assertRoutePolicyDomainAbsent(t *testing.T, f *attemptFixture) {
	t.Helper()
	if n := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'review_route_policy_%' OR tbl_name LIKE 'review_route_policy_%'`); n != 0 {
		t.Fatal("partial route-policy schema retained", n)
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM metadata WHERE key='schema:review_route_policy'`); n != 0 {
		t.Fatal("partial route-policy version retained", n)
	}
}

func routePolicySourceRows(t *testing.T, f *attemptFixture) map[string][]string {
	t.Helper()
	rows := comparisonSourceRows(t, f)
	for _, key := range []string{"source-schema", "metadata"} {
		selected := []string{}
		for _, record := range rows[key] {
			if strings.Contains(record, "review_route_policy_") || strings.Contains(record, "schema:review_route_policy") {
				continue
			}
			selected = append(selected, record)
		}
		rows[key] = selected
	}
	return rows
}

func routePolicyMigrationFixture(t *testing.T) (*attemptFixture, Attempt, []string) {
	t.Helper()
	f, a, evidence := routeFixture(t, 3)
	ids := []string{}
	for i := range int64(3) {
		ids = append(ids, routeCreate(t, f, evidence, "migration-route-"+string(rune('a'+i)), routeRequest(i, i, 4)).ID)
	}
	routeDecision(t, f, ids[0], "migration-rejected", 1, "rejected")
	routeDecision(t, f, ids[1], "migration-accepted", 1, "accepted")
	routeFix(t, f, ids[1], "migration-fixed", 2, "", strings.Repeat("f", 40))
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs`); n != 0 {
		t.Fatal("fixture did not preserve precompleted source", n)
	}
	return f, a, ids
}

func TestRoutePolicyMigrationPreservesAuthorityAndRestart(t *testing.T) {
	f, _, ids := routePolicyMigrationFixture(t)
	routePolicyPreDomain(t, f)
	before := routePolicySourceRows(t, f)
	if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, routePolicySourceRows(t, f)) {
		t.Fatal("migration changed predecessor authority or DDL")
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_route_policy_facts`); n != 3 {
		t.Fatal("incomplete backfill", n)
	}
	if n, err := routePending(t, f, 4); err != nil || n != 1 {
		t.Fatal("precompleted-source obligations", n, err)
	}
	facts := []routePolicyFact{}
	for _, id := range ids {
		fact, err := readRoutePolicyFact(t.Context(), f.store.DB(), id)
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, fact)
	}
	if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var file string
	if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	if reopened.InstanceID() != instance {
		t.Fatal("restart changed instance")
	}
	if err := EnsureSchema(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(reopened, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		got, err := readRoutePolicyFact(t.Context(), reopened.DB(), id)
		if err != nil || got != facts[i] {
			t.Fatal("restart changed derived fact", err)
		}
	}
	if !reflect.DeepEqual(before, routePolicySourceRows(t, f)) {
		t.Fatal("restart changed source rows")
	}
	if err := ValidateRoutePolicyFacts(t.Context(), reopened.DB()); err != nil {
		t.Fatal(err)
	}
	if n, err := routePending(t, f, 4); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestRoutePolicyMigrationPartialFailureRollsBackAndRetries(t *testing.T) {
	f, _, _ := routePolicyMigrationFixture(t)
	routePolicyPreDomain(t, f)
	before := routePolicySourceRows(t, f)
	ddl := append([]string{}, RoutePolicySchemaSQL()...)
	ddl = append(ddl, `CREATE TRIGGER route_policy_test_second BEFORE INSERT ON review_route_policy_facts WHEN (SELECT count(*) FROM review_route_policy_facts)>=1 BEGIN SELECT RAISE(ABORT,'synthetic route backfill refusal'); END`)
	inserted := 0
	err := f.store.EnsureSchemaWithBackfill(t.Context(), "review_route_policy", [][]string{ddl}, func(ctx context.Context, conn *sql.Conn) error {
		err := backfillRoutePolicyFacts(ctx, conn)
		if err != nil {
			if scanErr := conn.QueryRowContext(ctx, `SELECT count(*) FROM review_route_policy_facts`).Scan(&inserted); scanErr != nil {
				return scanErr
			}
		}
		return err
	})
	if err == nil || inserted != 1 {
		t.Fatal("not actual partially staged backfill", inserted, err)
	}
	assertRoutePolicyDomainAbsent(t, f)
	if !reflect.DeepEqual(before, routePolicySourceRows(t, f)) {
		t.Fatal("failed migration changed source")
	}
	if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal("retry failed", err)
	}
	if !reflect.DeepEqual(before, routePolicySourceRows(t, f)) {
		t.Fatal("retry changed source")
	}
	if n, err := routePending(t, f, 4); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestRoutePolicyMigrationCorruptionAndCancellationRefuseAtomically(t *testing.T) {
	for _, mode := range []string{"corrupt", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, _, ids := routePolicyMigrationFixture(t)
			routePolicyPreDomain(t, f)
			if mode == "corrupt" {
				if _, err := f.store.DB().Exec(`UPDATE review_finding_routes SET body_json='{}' WHERE id=?`, ids[0]); err != nil {
					t.Fatal(err)
				}
			}
			before := routePolicySourceRows(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			if err := ensureRoutePolicySchema(ctx, f.store); err == nil {
				t.Fatal("invalid migration accepted")
			}
			assertRoutePolicyDomainAbsent(t, f)
			if !reflect.DeepEqual(before, routePolicySourceRows(t, f)) {
				t.Fatal("failed startup repaired/discarded predecessor source")
			}
			if mode == "cancelled" {
				if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
					t.Fatal("retry after cancellation", err)
				}
			}
		})
	}
}
