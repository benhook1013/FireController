package reviewexec

import "testing"

func TestExecutionEightUpgradePreservesCurrentComparisonTriggers(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	// Reconstruct both genuine execution-v8 cardinality constraints, retaining
	// the independent comparison-v2 schema and its live invalidation triggers.
	restorePreCooldownAttemptSchema(t, f)
	for _, statement := range []string{
		`CREATE TABLE old_reservations(provider TEXT NOT NULL,repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,action_class TEXT NOT NULL,account TEXT NOT NULL,attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),PRIMARY KEY(provider,repository_id,pull_number,action_class),UNIQUE(provider,account,action_class))`,
		`INSERT INTO old_reservations SELECT * FROM review_execution_reservations`,
		`DROP TABLE review_execution_reservations`,
		`ALTER TABLE old_reservations RENAME TO review_execution_reservations`,
		`UPDATE metadata SET value='8' WHERE key='schema:review_execution'`,
	} {
		admissionSQL(t, f, statement)
	}
	for _, statement := range canonicalAttemptTriggerStatements() {
		admissionSQL(t, f, statement)
	}
	assertTriggers := func() {
		t.Helper()
		if n := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name LIKE 'review_comparison_dirty_review_execution_attempts_%'`); n != 3 {
			t.Fatal("attempt invalidation triggers missing", n)
		}
		var version string
		if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_comparison'`).Scan(&version); err != nil || version != "2" {
			t.Fatal("comparison marker changed", version, err)
		}
	}
	assertTriggers()
	before := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_evidence") + snapshotTable(t, f, "review_comparison_proofs")
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	assertTriggers()
	if after := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_evidence") + snapshotTable(t, f, "review_comparison_proofs"); before != after {
		t.Fatal("upgrade changed retained source/proof")
	}
	admissionSQL(t, f, `UPDATE review_execution_attempts SET revision=revision WHERE id=?`, a.ID)
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_comparison_proofs WHERE state='dirty'`); n != 1 {
		t.Fatal("attempt mutation did not invalidate proof", n)
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("restart", err)
	}
	assertTriggers()
}

func TestExecutionCurrentSchemaSkipsMigrationForeignKeyCheck(t *testing.T) {
	f := newAttemptFixture(t)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE unrelated_parent(id TEXT PRIMARY KEY)`,
		`CREATE TABLE unrelated_child(parent_id TEXT REFERENCES unrelated_parent(id))`,
		`INSERT INTO unrelated_child VALUES('missing')`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	// At an actual migration boundary, clearing deferred bookkeeping must not
	// approve genuine retained reference damage anywhere in the database.
	attemptCode(t, validateExecutionForeignKeys(t.Context(), conn), "schema_invalid")
	var enabled int
	if err := conn.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatal("enforcement changed", enabled, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "metadata")
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("current schema ran migration-only integrity check", err)
	}
	if snapshotTable(t, f, "metadata") != before || attemptCount(t, f.store, `SELECT count(*) FROM unrelated_child`) != 1 {
		t.Fatal("no-op startup rewrote unrelated history or markers")
	}
}
