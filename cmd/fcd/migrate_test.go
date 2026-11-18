package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for round := 0; round < 2; round++ {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"migrate", "--data-dir", dir}, &stdout, &stderr); code != 0 {
			t.Fatalf("round %d: code %d diagnostics %s", round, code, stderr.String())
		}
		var result map[string]string
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result["state"] != "migrated" || result["quick_check"] != "ok" {
			t.Fatalf("round %d: unexpected result %q %v", round, stdout.String(), err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Spot-check domains created by different packages in the shared sequence.
	for _, table := range []string{"jobs", "review_scheduling", "tracking_items", "operator_variable_history"} {
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing after migrate: %v", table, err)
		}
	}
}

func TestMigrateRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"migrate"}, {"migrate", "--data-dir", ""}, {"migrate", "--data-dir", t.TempDir(), "extra"}, {"migrate", "--data-dir", t.TempDir(), "--timeout", "0"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatalf("%v: code %d stdout %q", args, code, stdout.String())
		}
	}
}
