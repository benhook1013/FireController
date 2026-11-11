package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		"missing cli guard":          `DROP INDEX review_reservations_cli_account`,
		"missing hosted guard":       `DROP INDEX review_reservations_hosted_account_repository`,
		"global hosted guard":        `DROP INDEX review_reservations_hosted_account_repository; CREATE UNIQUE INDEX review_reservations_hosted_account_repository ON review_execution_reservations(provider,account,action_class) WHERE action_class='hosted'`,
		"disabled hosted guard":      `DROP INDEX review_reservations_hosted_account_repository; CREATE UNIQUE INDEX review_reservations_hosted_account_repository ON review_execution_reservations(provider,account,repository_id,action_class) WHERE action_class='unused'`,
		"widened cli guard":          `DROP INDEX review_reservations_cli_account; CREATE UNIQUE INDEX review_reservations_cli_account ON review_execution_reservations(provider,account,repository_id,action_class) WHERE action_class='cli'`,
		"extra unique index":         `CREATE UNIQUE INDEX unexpected_reservation_account ON review_execution_reservations(account)`,
		"changed primary key target": `PRAGMA foreign_keys=OFF; CREATE TABLE altered_reservations(provider TEXT NOT NULL,repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,action_class TEXT NOT NULL,account TEXT NOT NULL,attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),PRIMARY KEY(provider,account,repository_id,pull_number,action_class)); INSERT INTO altered_reservations SELECT provider,repository_id,pull_number,action_class,account,attempt_id FROM review_execution_reservations; DROP TABLE review_execution_reservations; ALTER TABLE altered_reservations RENAME TO review_execution_reservations; CREATE UNIQUE INDEX review_reservations_cli_account ON review_execution_reservations(provider,account,action_class) WHERE action_class='cli'; CREATE UNIQUE INDEX review_reservations_hosted_account_repository ON review_execution_reservations(provider,account,repository_id,action_class) WHERE action_class='hosted'`,
	} {
		t.Run(name, func(t *testing.T) {
			damaged := restoreModifiedBundle(t, bundle, statement)
			before := restoreTree(t, damaged)
			if _, err := Inspect(t.Context(), damaged); err == nil {
				t.Fatal("rehashed bundle lost reservation safety")
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			if _, err := Restore(t.Context(), damaged, destination); err == nil {
				t.Fatal("Restore accepted damaged reservation guards")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("rejected restore created a destination", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, damaged)) {
				t.Fatal("rejection changed retained bundle data")
			}
		})
	}
}

func TestAttemptIndexPhysicalGuardsRejectRehashedBundles(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	for name, statement := range map[string]string{
		"missing-live":                `DROP INDEX review_attempts_live_operation`,
		"missing-lookup":              `DROP INDEX review_attempts_operation`,
		"nonunique-live":              `DROP INDEX review_attempts_live_operation; CREATE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id) WHERE state IN ('preparing','posting','active','uncertain')`,
		"wrong-live-column":           `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(id) WHERE state IN ('preparing','posting','active','uncertain')`,
		"missing-uncertain":           `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id) WHERE state IN ('preparing','posting','active')`,
		"full-live":                   `DROP INDEX review_attempts_live_operation; CREATE UNIQUE INDEX review_attempts_live_operation ON review_execution_attempts(operation_id)`,
		"wrong-lookup-order":          `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(id,operation_id)`,
		"unique-lookup":               `DROP INDEX review_attempts_operation; CREATE UNIQUE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id)`,
		"unique-operation-constraint": `CREATE UNIQUE INDEX legacy_attempt_operation_unique ON review_execution_attempts(operation_id)`,
		"partial-lookup":              `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id) WHERE state='active'`,
		"collation-lookup":            `DROP INDEX review_attempts_operation; CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id COLLATE NOCASE,id)`,
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

func TestWaitPhysicalGuardsRejectRehashedBundles(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	var canonical string
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_operation_waits'`).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	for name, mutation := range map[string][2]string{
		"missing primary key":    {"operation_id TEXT PRIMARY KEY", "operation_id TEXT"},
		"changed operation type": {"operation_id TEXT", "operation_id INTEGER"},
		"changed baseline type":  {"baseline_attempt_id TEXT", "baseline_attempt_id INTEGER"},
		"changed retry type":     {"retry_not_before TEXT", "retry_not_before INTEGER"},
		"changed paused type":    {"paused INTEGER", "paused TEXT"},
		"nullable retry":         {"retry_not_before TEXT NOT NULL", "retry_not_before TEXT"},
		"nullable paused":        {"paused INTEGER NOT NULL", "paused INTEGER"},
		"missing retry default":  {"DEFAULT ''", ""},
		"changed retry default":  {"DEFAULT ''", "DEFAULT 'later'"},
		"missing paused default": {"DEFAULT 0", ""},
		"changed paused default": {"DEFAULT 0", "DEFAULT 1"},
		"missing paused check":   {"CHECK(paused IN(0,1))", ""},
		"weakened paused check":  {"CHECK(paused IN(0,1))", "CHECK(paused IN(0,1,2))"},
		"missing paired check":   {",CHECK((baseline_attempt_id IS NULL AND retry_not_before='') OR (baseline_attempt_id IS NOT NULL AND retry_not_before<>''))", ""},
		"weakened paired check":  {"baseline_attempt_id IS NULL AND retry_not_before=''", "baseline_attempt_id IS NULL"},
	} {
		t.Run(name, func(t *testing.T) {
			altered := strings.Replace(canonical, mutation[0], mutation[1], 1)
			if altered == canonical {
				t.Fatal("mutation did not change canonical schema")
			}
			damaged := restoreModifiedBundle(t, bundle, "DROP TABLE review_operation_waits; "+altered)
			before := restoreTree(t, damaged)
			if _, err := Inspect(t.Context(), damaged); err == nil {
				t.Error("Inspect accepted damaged wait constraints")
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			if _, err := Restore(t.Context(), damaged, destination); err == nil {
				t.Error("Restore accepted damaged wait constraints")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("rejected restore created a destination", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, damaged)) {
				t.Fatal("rejection changed retained bundle data")
			}
		})
	}
}
