package backup

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestCompletedChannelCountsBackupLegacyElevenAndTwelve(t *testing.T) {
	for _, version := range []string{"11", "12"} {
		t.Run(version, func(t *testing.T) {
			f := historyInventoryAPIFixture(t)
			clean := reviews.RunInput{SourceRunID: "completed-clean", Channel: "cli", HeadSHA: strings.Repeat("a", 40), Scope: "Synthetic clean comparison", Outcome: "completed", Complete: true, Attributable: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Findings: []reviews.Finding{}}
			failed := clean
			failed.SourceRunID, failed.Outcome = "failed-empty-capture", "failed"
			failed.Complete, failed.Attributable, failed.Finalized = false, false, false
			failed.CoverageLimits = []string{"The synthetic provider command failed before producing findings"}
			zero := int64(0)
			f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 991, Source: "completed-count-backup-proof", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 7, Title: "Retained failed and completed clean reviews", State: "open", HeadSHA: clean.HeadSHA, BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "pending", Mergeability: "unknown", Complete: true, Runs: []reviews.RunInput{failed, clean}}}})
			wantCLI := 1
			if version == "11" {
				// Twelve changes only these derived values. Reproduce the prior
				// eleven image before publishing, without rewriting run history.
				if _, err := f.store.DB().Exec("UPDATE metadata SET value='11' WHERE key='schema:reviews'; UPDATE review_current_totals SET cli_run_count=2 WHERE pull_number=7"); err != nil {
					t.Fatal(err)
				}
				wantCLI = 2
			}
			assertCompletedBackupCounts(t, f.store.DB(), version, wantCLI)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			published := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("mixed-outcome bundle refused", err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || restored.State != "inactive" {
				t.Fatal("inactive restore", restored, err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			assertCompletedBackupCounts(t, db, version, wantCLI)
			after := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && after[table] != rows {
					t.Fatalf("inactive restore changed retained table %s", table)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := core.Open(t.Context(), destination)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			preUpgrade := backupRows(t, upgraded.DB())
			if err := reviews.EnsureSchema(t.Context(), upgraded); err != nil {
				t.Fatal("restored derived cache migration", err)
			}
			assertCompletedBackupCounts(t, upgraded.DB(), "12", 1)
			if err := reviews.ValidateCurrentSummaries(t.Context(), upgraded.DB()); err != nil {
				t.Fatal(err)
			}
			postUpgrade := backupRows(t, upgraded.DB())
			for table, rows := range preUpgrade {
				if table != "metadata" && table != "review_current_totals" && postUpgrade[table] != rows {
					t.Fatalf("derived count migration changed retained table %s", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(published, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection, restore or restored upgrade changed source/bundle bytes")
			}
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("published predecessor no longer inspectable", err)
			}
		})
	}
}

func assertCompletedBackupCounts(t *testing.T, db *sql.DB, wantVersion string, wantCLI int) {
	t.Helper()
	var version string
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='schema:reviews'").Scan(&version); err != nil || version != wantVersion {
		t.Fatal("unexpected reviews version", version, err)
	}
	var raw, hosted, cli, subagent, manual int
	if err := db.QueryRow("SELECT run_count,hosted_run_count,cli_run_count,subagent_run_count,manual_run_count FROM review_current_totals WHERE pull_number=7").Scan(&raw, &hosted, &cli, &subagent, &manual); err != nil || raw != 2 || cli != wantCLI || hosted != 0 || subagent != 0 || manual != 0 {
		t.Fatal("raw inventory/completed channel totals", raw, hosted, cli, subagent, manual, err)
	}
	var failed, completed, findings int
	if err := db.QueryRow("SELECT sum(outcome='failed'),sum(outcome='completed'),sum(found) FROM review_run_versions WHERE pull_number=7").Scan(&failed, &completed, &findings); err != nil || failed != 1 || completed != 1 || findings != 0 {
		t.Fatal("failed and completed clean source not retained", failed, completed, findings, err)
	}
}
