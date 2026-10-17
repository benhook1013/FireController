package backup

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Construct a genuine empty predecessor directly from its published physical
// layout. No v5 startup, cache dropping, metadata relabelling or dynamic owner
// migration copying is involved; owner tests cover retained payload migration.
func TestCurrentSummaryInventoryAuthenticV4BackupRemainsSupported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source-v4")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = installLegacyReviewInventorySchema(t.Context(), store, 3); err != nil {
		t.Fatal(err)
	}
	statements := []string{`DROP TRIGGER review_runs_snapshot_revision_insert`, `DROP TRIGGER review_runs_snapshot_revision_update`, `DROP TABLE review_findings`, `DROP TABLE review_attempts`, `DROP TABLE review_runs`, `DROP TABLE review_retired_pulls`, `DROP TABLE review_pulls`}
	statements = append(statements, reviews.HistorySchemaSQL()...)
	for _, statement := range reviews.HistoryReadViewSQL() {
		statements = append(statements, statement)
	}
	if err = store.EnsureSchema(t.Context(), "reviews", []string{}, []string{}, []string{}, statements); err != nil {
		t.Fatal(err)
	}
	var version string
	if err = store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "4" {
		t.Fatal("not an authentic v4 fixture", version, err)
	}
	var caches int
	if err = store.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('review_current_totals','review_current_attempt_order')`).Scan(&caches); err != nil || caches != 0 {
		t.Fatal("v4 fixture contains v5 derived tables", caches, err)
	}
	before := backupRows(t, store.DB())
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-v4")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retained := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && retained[table] != rows {
			t.Fatalf("v4 restore changed %s", table)
		}
	}
	if _, err = validateSchema(t.Context(), db, true); err != nil {
		t.Fatal("restored v4 schema refused", err)
	}
	if !reflect.DeepEqual(before, backupRows(t, store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("v4 backup operations changed source/bundle")
	}
}

func currentSummaryInventoryFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := historyInventoryAPIFixture(t)
	start := "2026-01-01T01:00:00.000000001+01:00"
	finish := "2026-01-01T00:01:00.000000002Z"
	duration := int64(60)
	run := reviews.RunInput{SourceRunID: "cache-run", Channel: "cli", HeadSHA: strings.Repeat("a", 40), Scope: "Synthetic comparison", Outcome: "completed", Complete: false, StartedAt: start, FinishedAt: finish, CoverageLimits: []string{"Incomplete retained source"}, Findings: []reviews.Finding{}}
	pull := reviews.PullInput{Number: 7, Title: "Derived current summary source", State: "merged", HeadSHA: run.HeadSHA, BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: false, Runs: []reviews.RunInput{run}, Attempts: []reviews.Attempt{{SourceAttemptID: "cache-attempt", Channel: "cli", HeadSHA: run.HeadSHA, State: "completed", StartedAt: start, FinishedAt: finish, DurationSeconds: &duration, SourceRunID: run.SourceRunID}}}
	other := pull
	other.Number = 8
	other.Runs = nil
	other.Attempts = nil
	revision := int64(0)
	f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "cache-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull, other}, Retired: []reviews.RetiredPull{}})
	return f
}

func assertCurrentSummaryInventoryRefused(t *testing.T, bundle Summary, physicalAndFKValid bool) {
	t.Helper()
	if physicalAndFKValid {
		db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = validateSchema(t.Context(), db, false); err != nil {
			db.Close()
			t.Fatal("damage must retain canonical schema", err)
		}
		rows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if rows.Next() {
			rows.Close()
			db.Close()
			t.Fatal("damage must retain ordinary foreign keys")
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			db.Close()
			t.Fatal(err)
		}
		rows.Close()
		db.Close()
	}
	immutable := restoreTree(t, bundle.Path)
	_, err := Inspect(t.Context(), bundle.Path)
	backupCode(t, err, "backup_invalid")
	destination := filepath.Join(t.TempDir(), "must-not-create")
	_, err = Restore(t.Context(), bundle.Path, destination)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("invalid current summary reached restore writes")
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("failed inspection/restore changed retained bundle")
	}
}

func TestCurrentSummaryInventoryV5RehashedDerivedDamageRefused(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"run-count", `UPDATE review_current_totals SET run_count=run_count+1 WHERE pull_number=7`},
		{"attempt-count", `UPDATE review_current_totals SET attempt_count=attempt_count+1 WHERE pull_number=7`},
		{"incomplete-count", `UPDATE review_current_totals SET incomplete_run_count=0 WHERE pull_number=7`},
		{"sort-key", `UPDATE review_current_attempt_order SET sort_key='2026-01-01T00:01:00.000000001Z'`},
		{"wrong-pr", `UPDATE review_current_attempt_order SET pull_number=8`},
		{"wrong-channel", `UPDATE review_current_attempt_order SET channel='hosted'`},
		{"wrong-position", `UPDATE review_current_attempt_order SET position=position+1`},
		{"missing-totals", `DELETE FROM review_current_totals WHERE pull_number=7`},
		{"missing-order", `DELETE FROM review_current_attempt_order`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := currentSummaryInventoryFixture(t)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("valid current summaries refused", err)
			}
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("negative proof changed source")
			}
		})
	}
}

func TestCurrentSummaryInventoryV5CanonicalDerivedSchemaRefused(t *testing.T) {
	tests := []struct{ name, sql string }{
		{"missing-table", `DROP TABLE review_current_totals`},
		{"missing-index", `DROP INDEX review_current_attempt_order_latest`},
		{"wrong-index-order", `DROP INDEX review_current_attempt_order_latest; CREATE INDEX review_current_attempt_order_latest ON review_current_attempt_order(project_id,pull_number,channel,sort_key,position)`},
		{"unknown-index", `CREATE INDEX unexpected_current_cache ON review_current_totals(project_id)`},
	}
	for _, change := range []struct{ name, table, old, new string }{
		{"changed-count-check", "review_current_totals", "CHECK(run_count>=0)", "CHECK(run_count>=-1)"},
		{"changed-delete-action", "review_current_attempt_order", "ON DELETE CASCADE", "ON DELETE NO ACTION"},
		{"changed-primary-key", "review_current_attempt_order", "PRIMARY KEY(project_id,source,source_attempt_id)", "PRIMARY KEY(project_id,source_attempt_id)"},
	} {
		var definition string
		for _, statement := range reviews.CurrentSummarySchemaSQL() {
			if strings.HasPrefix(statement, "CREATE TABLE "+change.table+" (") {
				definition = statement
			}
		}
		altered := strings.Replace(definition, change.old, change.new, 1)
		if definition == "" || altered == definition {
			t.Fatal("malformed schema fixture did not alter owner DDL", change.name)
		}
		tests = append(tests, struct{ name, sql string }{change.name, `DROP TABLE ` + change.table + `; ` + altered})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := historyInventoryAPIFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
}
