package backup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewAttemptReasonNativeJournalBackupRetainsPinnedProjection(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	var attemptID string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_attempts WHERE state='preparing'`).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Advance(t.Context(), f.project, attemptID, 1, "held", "hosted_finished_inventory_unproven", ""); err != nil {
		t.Fatal(err)
	}
	pull := f.request(t, http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/2", nil)
	body, err := json.Marshal(pull)
	if err != nil || !bytes.Contains(body, []byte(`"reason":"hosted_finished_inventory_unproven"`)) {
		t.Fatal("journal diagnostic missing from current summary", err)
	}
	archive := f.request(t, http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/2/attempts?snapshot="+pull["snapshot_id"].(string), nil)
	body, err = json.Marshal(archive)
	if err != nil || !bytes.Contains(body, []byte(`"reason":"hosted_finished_inventory_unproven"`)) {
		t.Fatal("journal diagnostic missing from pinned archive", err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_execution_attempts", "review_execution_transitions", "review_operations", "review_attempt_versions", "review_attempt_memberships", "review_current_attempts", "review_pull_versions", "review_snapshots"} {
		if before[table] != after[table] {
			t.Fatal("journal/projection/pinned data changed", table)
		}
	}
}

func TestReviewAttemptReasonBackupRoundTripAndSchema7Inspection(t *testing.T) {
	for _, version := range []string{"7", "9"} {
		t.Run(version, func(t *testing.T) {
			f := historyInventoryAPIFixture(t)
			revision := int64(0)
			p := reviews.PullInput{Number: 1, Title: "Retained attempts", State: "open", CIState: "unknown", Mergeability: "unknown", Attempts: []reviews.Attempt{{SourceAttemptID: "older", Channel: "hosted", State: "uncertain", StartedAt: "2026-01-01T00:00:00Z"}, {SourceAttemptID: "latest", Channel: "hosted", State: "held", StartedAt: "2026-01-01T00:01:00Z"}}}
			if version == "9" {
				p.Attempts[0].Reason = "hosted_finished_inventory_unproven"
				p.Attempts[1].Reason = "repository_changed"
			}
			f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "attempt-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{p}})
			if version == "7" {
				removeChannelRunCountsForLegacyFixture(t, f.store)
				for _, statement := range []string{`DROP VIEW review_attempts`, `ALTER TABLE review_attempt_versions DROP COLUMN reason`, reviews.HistoryReadViewSQLV7()["review_attempts"], `UPDATE metadata SET value='7' WHERE key='schema:reviews'`} {
					if _, err := f.store.DB().Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("canonical predecessor/current inspect", err)
			}
			dest := filepath.Join(t.TempDir(), "inactive")
			if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			after := backupRows(t, db)
			for _, table := range []string{"review_attempt_versions", "review_attempt_memberships", "review_current_attempts", "review_pull_versions", "review_snapshots"} {
				if before[table] != after[table] {
					t.Fatal("retained attempt data changed", table)
				}
			}
			if version == "9" {
				if err := reviews.ValidateAttemptReasonsHistory(t.Context(), db); err != nil {
					t.Fatal(err)
				}
				var reason string
				if err := db.QueryRow(`SELECT reason FROM review_attempt_versions WHERE source_attempt_id='older'`).Scan(&reason); err != nil || reason != p.Attempts[0].Reason {
					t.Fatal("older nonwinning reason lost", reason, err)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection changed source/bundle")
			}
			if version == "9" {
				mutateBundleSQL(t, bundle.Path, `UPDATE review_attempt_versions SET reason=? WHERE source_attempt_id='older'`, strings.Repeat("unsafe/", 2))
				if _, err := Inspect(t.Context(), bundle.Path); err == nil {
					t.Fatal("rehashed nonwinning diagnostic corruption accepted")
				}
			}
		})
	}
}
