package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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

func TestComparisonProofSetRefusesMissingOrMismatchedAuthority(t *testing.T) {
	f, _, _ := comparisonMigrationFixture(t)
	before, proofs := comparisonSourceRows(t, f), comparisonMigrationProofs(t, f)
	if err := validateComparisonProofSet(t.Context(), f.store.DB()); err != nil {
		t.Fatal("valid comparison set refused", err)
	}
	for _, tc := range []struct{ name, mutation string }{
		{"missing", `DROP TRIGGER review_comparison_proofs_retain; DELETE FROM review_comparison_proofs`},
		{"orphan", `PRAGMA defer_foreign_keys=ON; UPDATE review_comparison_proofs SET operation_id='missing-operation' WHERE operation_id=(SELECT min(operation_id) FROM review_comparison_proofs)`},
		{"empty-identity", `PRAGMA defer_foreign_keys=ON; UPDATE review_comparison_proofs SET operation_id='' WHERE operation_id=(SELECT min(operation_id) FROM review_comparison_proofs)`},
		{"wrong-project", `UPDATE review_comparison_proofs SET project_id='exec-project-two'`},
		{"wrong-pull", `UPDATE review_comparison_proofs SET pull_number=pull_number+1`},
		{"wrong-channel", `UPDATE review_comparison_proofs SET channel=CASE channel WHEN 'cli' THEN 'hosted' ELSE 'cli' END`},
		{"noncompleted-operation", `UPDATE review_operations SET state='failed' WHERE state='completed'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked := false
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, tc.mutation); err != nil {
					return err
				}
				checked = true
				return validateComparisonProofSet(ctx, tx)
			})
			if !checked || err == nil {
				t.Fatal("set mismatch did not reach validation/refusal", checked, err)
			}
			if !reflect.DeepEqual(before, comparisonSourceRows(t, f)) || !reflect.DeepEqual(proofs, comparisonMigrationProofs(t, f)) {
				t.Fatal("refused set check changed retained authority or proofs")
			}
		})
	}
	// Set membership is insufficient for maintenance: a forged anchor has the
	// right owner but must still fail the exported full re-derivation boundary.
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE review_comparison_proofs SET candidate_sha=?`, strings.Repeat("e", 40)); err != nil {
			return err
		}
		if err := validateComparisonProofSet(ctx, tx); err != nil {
			t.Error("derived-value mutation unexpectedly changed the set", err)
			return err
		}
		return ValidateComparisonProofs(ctx, tx)
	})
	if err == nil || !reflect.DeepEqual(proofs, comparisonMigrationProofs(t, f)) {
		t.Fatal("full validation lost derived-value rejection/rollback", err)
	}
}

func TestComparisonMigrationSetMismatchRollsBack(t *testing.T) {
	f, _, _ := comparisonMigrationFixture(t)
	comparisonPreDomain(t, f)
	before := comparisonSourceRows(t, f)
	statements := append([]string{}, ComparisonSchemaSQL()...)
	statements = append(statements, `CREATE TRIGGER review_comparison_test_wrong_scope AFTER INSERT ON review_comparison_proofs BEGIN UPDATE review_comparison_proofs SET pull_number=pull_number+1 WHERE operation_id=NEW.operation_id; END;`)
	err := f.store.EnsureSchemaWithBackfill(t.Context(), "review_comparison", [][]string{statements}, backfillComparisonProofs)
	if err == nil {
		t.Fatal("migration accepted mismatched proof scope")
	}
	assertComparisonDomainAbsent(t, f)
	if !reflect.DeepEqual(before, comparisonSourceRows(t, f)) {
		t.Fatal("set mismatch changed retained source authority")
	}
	if err := ensureComparisonSchema(t.Context(), f.store); err != nil {
		t.Fatal("canonical retry after set refusal failed", err)
	}
}

// Build more than one migration page through real journal/capture/adjudication
// APIs. These are local synthetic captures, not live provider executions.
func comparisonMigrationRetainedHistory(t *testing.T) (*attemptFixture, Attempt, SourcePlan) {
	t.Helper()
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	source, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 130; i++ {
		next := coverageNextAttempt(t, f, fmt.Sprintf("migration-history-%d", i), "cli", strings.Repeat("9", 40))
		capture := source.Capture
		capture.ExpectedAnchor = *next.Anchor
		capture.CaptureSHA256 = fmt.Sprintf("%032x%032x", i+1, i+1)
		capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
		recorded, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
		if err != nil {
			t.Fatal(err)
		}
		adjudicationComplete(t, f, next)
		body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), fmt.Sprintf("migration-finalized-%d", i), recorded.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
		adjudicationReceipt(t, body, err, recorded.ID, "finalized", 1)
	}
	return f, a, current
}

func TestComparisonMigrationRetainedHistoryLongerBudgetAndRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a, current := comparisonMigrationRetainedHistory(t)
		proofs := comparisonMigrationProofs(t, f)
		if len(proofs) != 131 {
			t.Fatal("fixture did not cross the 128-operation migration page", len(proofs))
		}
		comparisonPreDomain(t, f)
		before := comparisonSourceRows(t, f)
		assertSource := func() {
			t.Helper()
			if !reflect.DeepEqual(comparisonSourceRows(t, f), before) {
				t.Fatal("startup changed retained source rows or DDL")
			}
		}
		migrations := [][]string{ComparisonSchemaSQL()}
		partial := func(ctx context.Context, conn *sql.Conn) error {
			if err := refreshComparisonProof(ctx, conn, attemptProject, proofs[0].operation); err != nil {
				return err
			}
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM review_comparison_proofs`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("first retained capture was not derived: count%d err%v", count, err)
			}
			return nil
		}
		calls := 0
		backfill := func(ctx context.Context, conn *sql.Conn) error {
			calls++
			if err := partial(ctx, conn); err != nil {
				return err
			}
			// Controlled elapsed work proves budgeting, not a throughput estimate.
			time.Sleep(31 * time.Second)
			return backfillComparisonProofs(ctx, conn)
		}
		err := f.store.EnsureSchemaWithBackfill(t.Context(), "review_comparison", migrations, backfill)
		attemptCode(t, err, "schema_migration_timeout")
		assertComparisonDomainAbsent(t, f)
		assertSource()
		var file string
		if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := core.Open(t.Context(), filepath.Dir(file), core.WithSchemaMigrationTimeout(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		f.store = reopened
		if err := reopened.EnsureSchemaWithBackfill(t.Context(), "review_comparison", migrations, backfill); err != nil || calls != 2 {
			t.Fatal("longer-budget retry failed identical retained history", calls, err)
		}
		assertSource()
		if !reflect.DeepEqual(comparisonMigrationProofs(t, f), proofs) {
			t.Fatal("longer-budget backfill changed canonical retained proofs")
		}
		if err := ValidateComparisonProofs(t.Context(), reopened.DB()); err != nil {
			t.Fatal(err)
		}
		// Rebind the engine to the reopened store for its ordinary coverage read.
		f.e, err = New(reopened, attemptCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		coverageComparison(t, f, a, current, ComparisonState{Covered: true})
		if err := reopened.EnsureSchemaWithBackfill(t.Context(), "review_comparison", migrations, backfill); err != nil || calls != 2 {
			t.Fatal("already-current startup reran retained backfill", calls, err)
		}
		assertSource()
		if !reflect.DeepEqual(comparisonMigrationProofs(t, f), proofs) {
			t.Fatal("repeat startup changed derived proofs")
		}

		// A longer budget must still honor cancellation after partial work.
		comparisonPreDomain(t, f)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err = reopened.EnsureSchemaWithBackfill(ctx, "review_comparison", migrations, func(ctx context.Context, conn *sql.Conn) error {
			if err := partial(ctx, conn); err != nil {
				return err
			}
			cancel()
			return nil // Even a callback omitting cancellation cannot commit.
		})
		attemptCode(t, err, "overloaded")
		assertComparisonDomainAbsent(t, f)
		assertSource()

		// Corrupt the last retained capture in migration order, so the real
		// backfill has processed a full page before discovering invalid authority.
		if _, err := reopened.DB().Exec(`UPDATE review_execution_evidence SET body_json='{}' WHERE operation_id=?`, proofs[len(proofs)-1].operation); err != nil {
			t.Fatal(err)
		}
		before = comparisonSourceRows(t, f)
		inserted := 0
		err = reopened.EnsureSchemaWithBackfill(t.Context(), "review_comparison", migrations, func(ctx context.Context, conn *sql.Conn) error {
			err := backfillComparisonProofs(ctx, conn)
			if scanErr := conn.QueryRowContext(ctx, `SELECT count(*) FROM review_comparison_proofs WHERE state='ready'`).Scan(&inserted); scanErr != nil {
				return scanErr
			}
			return err
		})
		if err == nil || inserted != 130 {
			t.Fatal("late corrupt capture did not refuse after a full page", inserted, err)
		}
		assertComparisonDomainAbsent(t, f)
		assertSource() // Corruption is retained for diagnosis, never repaired.
	})
}

func TestAttemptRebuildRepairRestoresInvalidationAndProofs(t *testing.T) {
	for _, stage := range []string{"schema-eight-upgrade", "already-schema-ten"} {
		t.Run(stage, func(t *testing.T) {
			f, a, id, _ := coverageFixture(t, 0, false)
			coverageFinalize(t, f, a, id, 0)
			for _, event := range []string{"insert", "update", "delete"} {
				admissionSQL(t, f, "DROP TRIGGER review_comparison_dirty_review_execution_attempts_"+event)
			}
			admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_comparison'`)
			// A retained dirty proof must be re-derived from unchanged valid source.
			admissionSQL(t, f, `UPDATE review_comparison_proofs SET state='dirty'`)
			if stage == "schema-eight-upgrade" {
				restorePreCooldownAttemptSchema(t, f)
				admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name LIKE 'review_comparison_dirty_review_execution_attempts_%'`); n != 3 {
				t.Fatal("attempt triggers lost", n)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs WHERE state='ready'`); n != 1 {
				t.Fatal("repair failed to rederive retained proof", n)
			}
			admissionSQL(t, f, `UPDATE review_execution_attempts SET revision=revision WHERE id=?`, a.ID)
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs WHERE state='dirty'`); n != 1 {
				t.Fatal("repaired trigger did not invalidate proof", n)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("idempotent repaired startup", err)
			}
		})
	}
}

func TestComparisonRepairReplacesStaleReadyProof(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	before := snapshotTable(t, f, "review_comparison_proofs")
	for _, event := range []string{"insert", "update", "delete"} {
		admissionSQL(t, f, "DROP TRIGGER review_comparison_dirty_review_execution_attempts_"+event)
	}
	admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_comparison'`)
	admissionSQL(t, f, `UPDATE review_comparison_proofs SET candidate_sha=? WHERE state='ready'`, strings.Repeat("f", 40))
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if snapshotTable(t, f, "review_comparison_proofs") != before {
		t.Fatal("stale ready proof not replaced from original authority")
	}
}
