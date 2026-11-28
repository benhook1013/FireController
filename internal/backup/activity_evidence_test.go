package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestActivityEvidenceSchemaBackupCompatibility(t *testing.T) {
	for _, version := range []string{"4", "5", "6"} {
		t.Run(version, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if version != "6" {
				if _, err := store.DB().Exec(`DROP TABLE review_cli_evidence_recovery; UPDATE metadata SET value='5' WHERE key='schema:review_evidence'`); err != nil {
					t.Fatal(err)
				}
			}
			if version == "4" {
				if err := store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `DROP TABLE review_activity_evidence; UPDATE metadata SET value='4' WHERE key='schema:review_evidence'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHostedSubmissionEvidenceSchemaBackupRestore(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT id,revision FROM review_execution_attempts WHERE state='preparing'`).Scan(&id, &revision); err != nil {
		t.Fatal(err)
	}
	query, err := e.ReadHostedAllowanceQuery(t.Context(), f.project, id)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: id, HeadSHA: query.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.Admit(t.Context(), f.project, id, revision, query.Anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Advance(t.Context(), f.project, id, a.Revision, "active", "trigger_recovered", "101")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	trigger := reviewprovider.HostedTriggerRecovery{State: "adopted", CommentID: 101, CreatedAt: now.Truncate(time.Second)}
	if err := e.RecordHostedSubmissionEvidence(t.Context(), f.project, id, query.Anchor, trigger, now); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var submitted string
	if err := db.QueryRow(`SELECT submitted_at FROM review_hosted_submission_evidence WHERE attempt_id=? AND trigger_id='101'`, id).Scan(&submitted); err != nil || submitted != trigger.CreatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("restore lost request timestamp: %s %v", submitted, err)
	}
	bad := restoreBundle(t, f)
	mutateBundleSQL(t, bad.Path, `UPDATE review_hosted_submission_evidence SET trigger_id='102'`)
	if _, err := Inspect(t.Context(), bad.Path); err == nil {
		t.Fatal("accepted a receipt for a different trigger")
	}
	legacy := restoreBundle(t, f)
	mutateBundleSQL(t, legacy.Path, `DROP TABLE review_hosted_submission_evidence; DELETE FROM metadata WHERE key='schema:review_hosted_submission_evidence'`)
	if _, err := Inspect(t.Context(), legacy.Path); err != nil {
		t.Fatal("pre-domain backup no longer supported", err)
	}
	legacyDestination := filepath.Join(t.TempDir(), "legacy-inactive")
	if _, err := Restore(t.Context(), legacy.Path, legacyDestination); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), legacyDestination)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := reviewexec.EnsureEvidenceSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_hosted_submission_evidence'`).Scan(&version); err != nil || version != "1" {
		t.Fatalf("canonical migration did not install the diagnostic domain: %q %v", version, err)
	}
	var rows int
	if err := store.DB().QueryRow(`SELECT count(*) FROM review_hosted_submission_evidence`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("migration guessed submission evidence: %d %v", rows, err)
	}
}
