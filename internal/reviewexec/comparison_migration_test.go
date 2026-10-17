package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// This models the genuine predecessor of the new ADDITIVE comparison domain.
// All pre-existing journal/evidence/adjudication schemas retain their exact DDL
// and data. No older physical schema is manufactured by relabeling its version.
func comparisonPreDomain(t *testing.T, f *attemptFixture) {
	t.Helper()
	objects := map[string][]string{}
	for _, statement := range ComparisonSchemaSQL() {
		fields := strings.Fields(statement)
		if len(fields) < 3 || fields[0] != "CREATE" {
			t.Fatal("unexpected comparison-owned DDL")
		}
		objects[fields[1]] = append(objects[fields[1]], fields[2])
	}
	for _, kind := range []string{"TRIGGER", "INDEX", "TABLE"} {
		for _, name := range objects[kind] {
			if _, err := f.store.DB().Exec("DROP " + kind + " " + name); err != nil {
				t.Fatal("remove additive comparison object", kind, err)
			}
		}
	}
	if _, err := f.store.DB().Exec(`DELETE FROM metadata WHERE key='schema:review_comparison'`); err != nil {
		t.Fatal(err)
	}
	assertComparisonDomainAbsent(t, f)
}

func assertComparisonDomainAbsent(t *testing.T, f *attemptFixture) {
	t.Helper()
	if n := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'review_comparison_%'`); n != 0 {
		t.Fatal("failed startup retained comparison DDL", n)
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM metadata WHERE key='schema:review_comparison'`); n != 0 {
		t.Fatal("failed startup published comparison version", n)
	}
}

// Follow the existing projectionDigest pattern, retaining exact complete row
// encodings rather than merely aggregate counts. These are bounded synthetic
// fixtures; failure output never dumps capture, receipt or finding bodies.
func comparisonSourceRows(t *testing.T, f *attemptFixture) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"source-schema": `SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'review_comparison_%' AND tbl_name NOT LIKE 'review_comparison_%' ORDER BY type,name`,
		"metadata":      `SELECT * FROM metadata WHERE key!='schema:review_comparison'`,
	}
	for _, table := range []string{"projects", "credentials", "grants", "review_execution_config", "review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_transitions", "review_execution_evidence", "review_hosted_observations", "review_adjudication_state", "review_adjudication_changes", "review_finding_routes", "review_finding_route_revisions", "review_policy_config", "events", "receipts"} {
		queries[table] = "SELECT * FROM " + table
	}
	out := map[string][]string{}
	for name, query := range queries {
		rows, err := f.store.DB().Query(query)
		if err != nil {
			t.Fatal("source conservation query", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		values := []string{}
		for rows.Next() {
			record, ptrs := make([]any, len(columns)), make([]any, len(columns))
			for i := range record {
				ptrs[i] = &record[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			body, err := json.Marshal(record)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			values = append(values, string(body))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(values)
		out[name] = values
	}
	return out
}

func comparisonMigrationProofs(t *testing.T, f *attemptFixture) []comparisonProof {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT ` + comparisonProofColumns + ` FROM review_comparison_proofs ORDER BY operation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []comparisonProof{}
	for rows.Next() {
		proof, err := scanComparisonProof(rows)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, proof)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func comparisonMigrationFixture(t *testing.T) (*attemptFixture, Attempt, SourcePlan) {
	t.Helper()
	f, a, id, current := coverageFixture(t, 1, false)
	routed := routeCreate(t, f, id, "migration-route", routeRequest(0, 0, 4))
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "migration-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	routeDecision(t, f, routed.ID, "migration-receiver-reject", 1, "rejected")
	routeDecision(t, f, routed.ID, "migration-receiver-reverse", 2, "accepted")
	routeFix(t, f, routed.ID, "migration-receiver-fix", 3, "", strings.Repeat("f", 40))
	comparisonCompleteMissingCapture(t, f, "migration-old-cli-missing", "cli", strings.Repeat("9", 40))
	comparisonCompleteMissingCapture(t, f, "migration-old-hosted-missing", "hosted", strings.Repeat("8", 40))
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	return f, a, current
}

func TestComparisonMigrationBackfillPreservesAuthorityAndRestart(t *testing.T) {
	f, a, current := comparisonMigrationFixture(t)
	comparisonPreDomain(t, f)
	before := comparisonSourceRows(t, f)
	if err := ensureComparisonSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
		t.Fatal("backfill rewrote predecessor source rows or DDL")
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs`); n != 3 {
		t.Fatal("backfill dropped/created native operation proof", n)
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs WHERE state='ready'`); n != 1 {
		t.Fatal("backfill lost current receiver proof or granted missing capture credit", n)
	}
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	if err := ValidateComparisonProofs(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	proofs := comparisonMigrationProofs(t, f)
	if err := ensureComparisonSchema(t.Context(), f.store); err != nil || !reflect.DeepEqual(comparisonSourceRows(t, f), before) || !reflect.DeepEqual(comparisonMigrationProofs(t, f), proofs) {
		t.Fatal("already-current startup changed authority", err)
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
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) || !reflect.DeepEqual(comparisonMigrationProofs(t, f), proofs) {
		t.Fatal("restart rewrote source authority or derived rows")
	}
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
}

func TestComparisonMigrationPartialBackfillFailureRollsBackAndRetries(t *testing.T) {
	f, a, current := comparisonMigrationFixture(t)
	comparisonPreDomain(t, f)
	before := comparisonSourceRows(t, f)
	statements := append([]string{}, ComparisonSchemaSQL()...)
	statements = append(statements, `CREATE TRIGGER review_comparison_test_fail_second BEFORE INSERT ON review_comparison_proofs WHEN (SELECT count(*) FROM review_comparison_proofs)>=1 BEGIN SELECT RAISE(ABORT,'synthetic comparison backfill refusal'); END;`)
	inserted := 0
	err := f.store.EnsureSchemaWithBackfill(t.Context(), "review_comparison", [][]string{statements}, func(ctx context.Context, conn *sql.Conn) error {
		err := backfillComparisonProofs(ctx, conn)
		if err != nil {
			if scanErr := conn.QueryRowContext(ctx, `SELECT count(*) FROM review_comparison_proofs`).Scan(&inserted); scanErr != nil {
				return scanErr
			}
		}
		return err
	})
	if err == nil || inserted != 1 {
		t.Fatal("fault did not interrupt actual partial backfill", inserted, err)
	}
	assertComparisonDomainAbsent(t, f)
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
		t.Fatal("failed backfill rewrote source rows or DDL")
	}
	if err := ensureComparisonSchema(t.Context(), f.store); err != nil {
		t.Fatal("retry did not release startup admission", err)
	}
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
		t.Fatal("retry rewrote source authority")
	}
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
}

func TestComparisonMigrationCorruptAuthorityRefusesWithoutRepair(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	comparisonPreDomain(t, f)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json='{}' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	before := comparisonSourceRows(t, f)
	if err := ensureComparisonSchema(t.Context(), f.store); err == nil {
		t.Fatal("corrupt predecessor capture granted startup credit")
	}
	assertComparisonDomainAbsent(t, f)
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
		t.Fatal("corrupt authority was silently repaired or discarded")
	}
}

func TestComparisonMigrationEarlierCancellationLeavesDomainAbsent(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	comparisonPreDomain(t, f)
	before := comparisonSourceRows(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ensureComparisonSchema(ctx, f.store); err == nil {
		t.Fatal("cancelled startup published comparison domain")
	}
	assertComparisonDomainAbsent(t, f)
	if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
		t.Fatal("cancelled startup changed source")
	}
	if err := ensureComparisonSchema(t.Context(), f.store); err != nil {
		t.Fatal("retry after cancellation failed", err)
	}
	coverageComparison(t, f, a, current, ComparisonState{Covered: true})
}
