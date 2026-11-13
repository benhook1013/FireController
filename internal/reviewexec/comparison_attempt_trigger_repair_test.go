package reviewexec

import (
	"strings"
	"testing"
)

func canonicalAttemptTriggerStatements() []string {
	var result []string
	for _, statement := range ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_dirty_review_execution_attempts_") {
			result = append(result, statement)
		}
	}
	return result
}
func assertCanonicalAttemptTriggers(t *testing.T, f *attemptFixture) {
	t.Helper()
	statements := canonicalAttemptTriggerStatements()
	if len(statements) != 3 {
		t.Fatal("expected exactly three attempt invalidators")
	}
	for _, statement := range statements {
		var kind, table, definition string
		name := strings.Fields(statement)[2]
		if err := f.store.DB().QueryRow(`SELECT type,tbl_name,sql FROM sqlite_schema WHERE name=?`, name).Scan(&kind, &table, &definition); err != nil || kind != "trigger" || table != "review_execution_attempts" || strings.TrimSuffix(strings.TrimSpace(definition), ";") != strings.TrimSuffix(strings.TrimSpace(statement), ";") {
			t.Fatal("canonical attempt invalidation absent/changed", name, err)
		}
	}
}
func TestComparisonAttemptTriggersSurviveSchemaEightUpgradeAndRepairCommittedNine(t *testing.T) {
	for _, scenario := range []string{"existing eight", "already nine missing triggers"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, evidence, _ := coverageFixture(t, 0, false)
			coverageFinalize(t, f, a, evidence, 0)
			assertCanonicalAttemptTriggers(t, f)
			if scenario == "existing eight" {
				restorePreCooldownAttemptSchema(t, f)
				// A real schema8 installation owns all three triggers before rebuilding.
				for _, statement := range canonicalAttemptTriggerStatements() {
					admissionSQL(t, f, statement)
				}
				assertCanonicalAttemptTriggers(t, f)
				admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
			} else {
				for _, statement := range canonicalAttemptTriggerStatements() {
					admissionSQL(t, f, `DROP TRIGGER `+strings.Fields(statement)[2])
				}
			}
			before := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_execution_evidence") + snapshotTable(t, f, "review_comparison_proofs")
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("comparison trigger restoration", err)
			}
			assertCanonicalAttemptTriggers(t, f)
			if before != snapshotTable(t, f, "review_execution_attempts")+snapshotTable(t, f, "review_execution_transitions")+snapshotTable(t, f, "review_execution_evidence")+snapshotTable(t, f, "review_comparison_proofs") {
				t.Fatal("trigger restoration changed retained authority/proofs")
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("idempotent restoration", err)
			}
			var state string
			if err := f.store.DB().QueryRow(`SELECT state FROM review_comparison_proofs WHERE operation_id=?`, a.OperationID).Scan(&state); err != nil || state != "ready" {
				t.Fatal("fixture did not retain actual ready proof", state, err)
			}
			admissionSQL(t, f, `UPDATE review_execution_attempts SET account=account WHERE id=?`, a.ID)
			if err := f.store.DB().QueryRow(`SELECT state FROM review_comparison_proofs WHERE operation_id=?`, a.OperationID).Scan(&state); err != nil || state != "dirty" {
				t.Fatal("restored attempt mutation did not invalidate proof", state, err)
			}
		})
	}
}
func TestComparisonAttemptTriggerRepairRejectsDifferentDefinitionAtomically(t *testing.T) {
	f := newAttemptFixture(t)
	statements := canonicalAttemptTriggerStatements()
	first, second := strings.Fields(statements[0])[2], strings.Fields(statements[1])[2]
	admissionSQL(t, f, `DROP TRIGGER `+first)
	admissionSQL(t, f, `DROP TRIGGER `+second)
	admissionSQL(t, f, `CREATE TRIGGER `+second+` AFTER UPDATE ON review_execution_attempts BEGIN SELECT 1; END`)
	before := snapshotTable(t, f, "sqlite_schema")
	err := EnsureSchema(t.Context(), f.store)
	attemptCode(t, err, "schema_invalid")
	if before != snapshotTable(t, f, "sqlite_schema") {
		t.Fatal("failed repair changed existing definitions or partially installed missing trigger")
	}
}
