package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReservationScopeBackupRestoreAndPhysicalGuards(t *testing.T) {
	f := newRestoreFixture(t)
	var version int
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("reservation schema not current: %d %v", version, err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("canonical scoped reservation refused", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_execution_reservations", "review_execution_attempts", "review_operations", "review_execution_transitions", "review_hosted_allowance_queries", "receipts"} {
		if before[table] != after[table] {
			t.Fatal("inactive restore changed native identity", table)
		}
	}
	for name, statement := range map[string]string{
		"missing cli guard":     `DROP INDEX review_reservations_cli_account`,
		"global hosted guard":   `DROP INDEX review_reservations_hosted_account_repository; CREATE UNIQUE INDEX review_reservations_hosted_account_repository ON review_execution_reservations(provider,account,action_class) WHERE action_class='hosted'`,
		"disabled hosted guard": `DROP INDEX review_reservations_hosted_account_repository; CREATE UNIQUE INDEX review_reservations_hosted_account_repository ON review_execution_reservations(provider,account,repository_id,action_class) WHERE action_class='unused'`,
	} {
		t.Run(name, func(t *testing.T) {
			damaged := restoreModifiedBundle(t, bundle, statement)
			if _, err := Inspect(t.Context(), damaged); err == nil {
				t.Fatal("rehashed bundle lost reservation safety")
			}
		})
	}
}

func TestAttemptIndexPhysicalGuardsRejectRehashedBundles(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	for name, statement := range map[string]string{
		"missing-live":       `DROP INDEX review_attempts_live_operation`,
		"missing-lookup":     `DROP INDEX review_attempts_operation`,
		"nonunique-live":     `DROP INDEX review_attempts_live_operation; CREATE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id) WHERE state IN ('preparing','posting','active','uncertain')`,
		"wrong-live-column":  `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(id) WHERE state IN ('preparing','posting','active','uncertain')`,
		"missing-uncertain":  `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id) WHERE state IN ('preparing','posting','active')`,
		"full-live":          `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id)`,
		"wrong-lookup-order": `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(id,operation_id)`,
		"unique-lookup":      `DROP INDEX review_attempts_operation; CREATE UNIQUE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id)`,
		"partial-lookup":     `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id) WHERE state='active'`,
		"collation-lookup":   `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id COLLATE NOCASE,id)`,
	} {
		t.Run(name, func(t *testing.T) {
			damaged := restoreModifiedBundle(t, bundle, statement)
			if _, err := Inspect(t.Context(), damaged); err == nil {
				t.Error("Inspect accepted damaged attempt indexes")
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			if _, err := Restore(t.Context(), damaged, destination); err == nil {
				t.Error("Restore accepted damaged attempt indexes")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("rejected restore created a destination", err)
			}
		})
	}
}
