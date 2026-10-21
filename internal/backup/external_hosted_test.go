package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func externalHostedInventoryFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := currentSummaryInventoryFixture(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := reviews.InitializeExternalHostedFrontier(ctx, tx, f.project, 991, 7)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	run := reviews.RunInput{SourceRunID: "run:00000000-0000-0000-0000-000000000001", Channel: "hosted", HeadSHA: strings.Repeat("c", 40), Scope: "Original external source comparison", Outcome: "completed", Complete: true, Attributable: true, StartedAt: "2026-10-20T00:00:00Z", FinishedAt: "2026-10-20T00:01:00Z", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "github-comment-1", Title: "Original external finding", Detail: "Literal retained provider detail", Severity: "medium"}}}
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, changed, err := reviews.PublishExternalHosted(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-observer"}, 991, 7, []reviews.RunInput{run})
		if err == nil && !changed {
			t.Fatal("external fixture must append")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func TestExternalHostedCanonicalBackupAndInactiveRestore(t *testing.T) {
	f := externalHostedInventoryFixture(t)
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-external-hosted")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" {
		t.Fatal(restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatal("restore changed source", table)
		}
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM review_hosted_discovery_state`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var detail, head string
	var finalized, hosted int
	if err = db.QueryRow(`SELECT detail FROM review_finding_versions WHERE source_finding_id='github-comment-1'`).Scan(&detail); err != nil || detail != "Literal retained provider detail" {
		t.Fatal(detail, err)
	}
	if err = db.QueryRow(`SELECT head_sha,finalized FROM review_run_versions WHERE source='github-hosted-observer'`).Scan(&head, &finalized); err != nil || head != strings.Repeat("c", 40) || finalized != 0 {
		t.Fatal(head, finalized, err)
	}
	if err = db.QueryRow(`SELECT hosted_run_count FROM review_current_totals WHERE pull_number=7`).Scan(&hosted); err != nil || hosted != 1 {
		t.Fatal(hosted, err)
	}
	if err = reviews.ValidateExternalHostedFrontiers(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup proof changed original source")
	}
}
func TestExternalHostedRehashedFrontierAndEvidenceIndexCorruptionRefused(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		physical  bool
	}{
		{"malformed-cutoff", `UPDATE review_hosted_discovery_state SET cutoff_at='invalid'`, true},
		{"noncanonical-cutoff", `UPDATE review_hosted_discovery_state SET cutoff_at='2026-10-20T01:00:00+01:00'`, true},
		{"oversized-cutoff", `UPDATE review_hosted_discovery_state SET cutoff_at=hex(zeroblob(16384))`, true},
		{"missing-initialized", `UPDATE review_hosted_discovery_state SET initialized_at=''`, true},
		{"frontier-index", `DROP INDEX review_run_versions_external_source; CREATE INDEX review_run_versions_external_source ON review_run_versions(source_run_id,project_id)`, false},
		{"evidence-index", `DROP INDEX review_evidence_hosted_run; CREATE INDEX review_evidence_hosted_run ON review_execution_evidence(project_id,body_json)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := externalHostedInventoryFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, tc.physical)
		})
	}
}
func TestExternalHostedAuthenticSchemaNineBackupRemainsSupported(t *testing.T) {
	testAuthenticChannelCountSchemaBackup(t, 9)
}
func TestExternalHostedEvidenceThreeBackupRemainsSupported(t *testing.T) {
	f := externalHostedInventoryFixture(t)
	if _, err := f.store.DB().Exec(`DROP INDEX review_evidence_hosted_publication; DROP INDEX review_evidence_hosted_run; UPDATE metadata SET value='3' WHERE key='schema:review_evidence'`); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-evidence-three")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	versions, err := validateSchema(t.Context(), db, true)
	if err != nil || versions["review_evidence"] != 3 {
		t.Fatal(versions, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("old evidence proof rewrote source")
	}
}
