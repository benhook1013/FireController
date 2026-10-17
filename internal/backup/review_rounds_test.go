package backup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Grant before real acceptance; complete the actual journal/capture through
// ordinary native APIs. Imported review rows cannot supply this completion.
func roundInventoryFixture(t *testing.T, late bool) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	zero, min, max := int64(0), int64(0), int64(4)
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "round-inventory-grant", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &min, Maximum: &max}); err != nil {
		t.Fatal(err)
	}
	bundleNativeReferences(t, f)
	var attempt, evidence string
	var revision int64
	if err = f.store.DB().QueryRow(`SELECT a.id,a.revision,e.id FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id JOIN review_execution_evidence e ON e.attempt_id=a.id WHERE o.channel='cli'`).Scan(&attempt, &revision, &evidence); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), f.project, attempt, revision, "completed", "captured_native_result", ""); err != nil {
		t.Fatal(err)
	}
	finalize := func() {
		t.Helper()
		if _, err = e.FinalizeReviewEvidence(t.Context(), scope, "round-inventory-finalize", evidence, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	if !late {
		finalize()
	}
	one := int64(1)
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "round-inventory-close", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	if late {
		two := int64(2)
		if _, err = e.ConfigureRoundControl(t.Context(), scope, "round-inventory-reopen", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &two, Action: "grant", Minimum: &min, Maximum: &max}); err != nil {
			t.Fatal(err)
		}
		finalize()
	}
	c, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if late {
		if c.Epoch != 2 || c.Used != 0 || c.Dry != 0 || c.Closed {
			t.Fatal("late old grant changed new grant", c)
		}
	} else if c.Epoch != 1 || c.Used != 1 || c.Dry != 1 || !c.Closed {
		t.Fatal("completed closed grant not retained", c)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine rounds failed validation", err)
	}
	return f
}

func TestRoundInventoryNativeCompletionAndLateEpochRoundTrip(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed-current", true: "late-old-epoch"}[late], func(t *testing.T) {
			f := roundInventoryFixture(t, late)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-rounds")
			if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			after := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && after[table] != rows {
					t.Fatalf("restore changed retained table %s", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("read/restore changed source or immutable bundle")
			}
		})
	}
}

func TestRoundInventoryResealedProgressAndAuthorityRefused(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing-ledger", `DELETE FROM review_round_completions`},
		{"missing-binding", `DELETE FROM review_round_completions; DELETE FROM review_round_operation_bindings`},
		{"false-accepted", `UPDATE review_round_completions SET accepted=1`},
		{"wrong-ordinal", `UPDATE review_round_completions SET ordinal=2`},
		{"false-progress", `UPDATE review_round_controls SET used=2,dry=2`},
		{"false-dry", `UPDATE review_round_controls SET dry=0`},
		{"wrong-completion-time", `UPDATE review_round_completions SET completed_at='unknown'`},
		{"wrong-current-time", `UPDATE review_round_controls SET updated_at='2026-01-01T00:00:00Z'`},
		{"wrong-config", `UPDATE review_round_controls SET maximum=5`},
		{"audit-gap", `DELETE FROM review_round_changes WHERE revision=2; UPDATE review_round_controls SET revision=1,closed=0`},
		{"audit-noncanonical", `UPDATE review_round_changes SET body_json=body_json||' '`},
		{"blob-audit", `UPDATE review_round_changes SET body_json=CAST(body_json AS BLOB)`},
		{"blob-actor", `UPDATE review_round_controls SET actor_id=CAST(actor_id AS BLOB)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := roundInventoryFixture(t, false)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("refused bundle changed source")
			}
		})
	}
}

func TestRoundInventoryExactPhysicalSchemaAndLegacyOptionality(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing-scope-index", `DROP INDEX review_round_operations_scope`},
		{"wrong-binding-index", `DROP INDEX review_round_bindings_scope; CREATE INDEX review_round_bindings_scope ON review_round_operation_bindings(operation_id)`},
		{"unexpected-index", `CREATE INDEX arbitrary_round_credit ON review_round_completions(accepted)`},
		{"extra-column", `ALTER TABLE review_round_completions ADD COLUMN invented_credit INTEGER`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
	for _, name := range []string{"review_round_operation_bindings", "review_round_completions"} {
		t.Run("nullable-"+name, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			var changed string
			for _, statement := range reviewexec.RoundSchemaSQL() {
				if strings.HasPrefix(statement, "CREATE TABLE "+name+"(") {
					changed = strings.Replace(statement, "operation_id TEXT NOT NULL PRIMARY KEY", "operation_id TEXT PRIMARY KEY", 1)
					if changed == statement {
						t.Fatal("canonical NOT NULL operation identity missing")
					}
				}
			}
			if changed == "" {
				t.Fatal("canonical table missing")
			}
			query := "DROP TABLE " + name + ";" + changed
			if name == "review_round_operation_bindings" {
				for _, statement := range reviewexec.RoundSchemaSQL() {
					if strings.HasPrefix(statement, "CREATE INDEX review_round_bindings_scope ") {
						query += statement
					}
				}
			}
			mutateBundleSQL(t, bundle.Path, query)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
	t.Run("predecessor-without-domain", func(t *testing.T) {
		f := newRestoreFixture(t)
		var adoptions int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_round_adoptions`).Scan(&adoptions); err != nil || adoptions != 0 {
			t.Fatal("predecessor adoption fixture not empty", err)
		}
		if _, err := f.store.DB().Exec(`DROP TABLE review_round_adoptions; DELETE FROM metadata WHERE key='schema:review_round_adoption'`); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"review_round_completions", "review_round_operation_bindings", "review_round_controls", "review_round_changes"} {
			var count int
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatal("predecessor fixture not empty", err)
			}
			if _, err := f.store.DB().Exec("DROP TABLE " + table); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.store.DB().Exec(`DROP INDEX review_round_operations_scope; DELETE FROM metadata WHERE key='schema:review_rounds'`); err != nil {
			t.Fatal(err)
		}
		bundle := restoreBundle(t, f)
		if _, err := Inspect(t.Context(), bundle.Path); err != nil {
			t.Fatal("supported older domain set refused", err)
		}
	})
}

func TestRoundInventoryCancellationIsReadOnly(t *testing.T) {
	f := roundInventoryFixture(t, true)
	before := backupRows(t, f.store.DB())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reviewexec.ValidateRoundSnapshot(ctx, f.store.DB()); err == nil {
		t.Fatal("canceled validation succeeded")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("canceled validation changed authority")
	}
	// Canonical bindings cannot hide a NULL operation from keyset traversal.
	if _, err := f.store.DB().Exec(`INSERT INTO review_round_operation_bindings(operation_id,project_id,pull_number,channel,epoch,grant_revision) VALUES(NULL,?,4,'cli',1,1)`, f.project); err == nil {
		t.Fatal("canonical binding accepted NULL")
	}
	if _, err := f.store.DB().Exec(`INSERT INTO review_round_completions(operation_id,evidence_id,project_id,pull_number,channel,epoch,ordinal,accepted,completed_at) VALUES(NULL,'evidence',?,4,'cli',1,1,0,'2026-01-01T00:00:00Z')`, f.project); err == nil {
		t.Fatal("canonical completion accepted NULL")
	}
}

func TestRoundInventoryAcceptedPendingCaptureCountsWithoutReadyCredit(t *testing.T) {
	f := roundInventoryFixture(t, false)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	two, min, max := int64(2), int64(0), int64(3)
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "pending-round-reopen", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &two, Action: "grant", Minimum: &min, Maximum: &max}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(t.Context(), scope, "pending-round-operation", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("invalid operation receipt")
	}
	a, err := e.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	anchor := reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 4, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: at.Format(time.RFC3339Nano)}
	a, err = e.Admit(t.Context(), f.project, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	captures, err := reviewruntime.NewCaptureStore(filepath.Join(f.dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := captures.Save(t.Context(), a.ID, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("Synthetic pending accepted source finding"), Started: true}, StartedAt: at, FinishedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := e.RecordReviewEvidence(t.Context(), f.project, a.ID, reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 1, Findings: []reviewprovider.ParsedFinding{{Title: "Accepted pending source correction", Detail: "Retained original finding", Severity: "low"}}, ReviewedFiles: []string{"example.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "captured_native_result", ""); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err = e.DecideReviewFinding(t.Context(), scope, "pending-round-accept", ev.ID, reviewexec.FindingDecisionRequest{ExpectedRevision: &zero, Finding: 0, Disposition: "accepted", Reason: "Verified correction still pending"}); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err = e.FinalizeReviewEvidence(t.Context(), scope, "pending-round-finalize", ev.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &one}); err != nil {
		t.Fatal(err)
	}
	c, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil || c.Epoch != 2 || c.Used != 1 || c.Dry != 0 {
		t.Fatal("accepted source round not counted", c, err)
	}
	plan := reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: 991, FullName: "synthetic/restore", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 4, RepositoryID: 991, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: anchor.ObservedAt}
	comparison, err := e.Comparison(t.Context(), f.project, plan, "cli")
	if err != nil || !comparison.Pending {
		t.Fatal("pending source proof lost", comparison, err)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("validator confused pending proof with source count", err)
	}
	bundle := restoreBundle(t, f)
	destination := filepath.Join(t.TempDir(), "inactive-pending-round")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
		t.Fatal(err)
	}
}

func TestRoundInventoryExistingNativeCaptureIsNotBackfilled(t *testing.T) {
	f := comparisonInventoryFixture(t, true)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	zero, min, max := int64(0), int64(0), int64(4)
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "after-existing-capture", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &min, Maximum: &max}); err != nil {
		t.Fatal(err)
	}
	var bindings, completions int
	if err = f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_round_operation_bindings),(SELECT count(*) FROM review_round_completions)`).Scan(&bindings, &completions); err != nil || bindings != 0 || completions != 0 {
		t.Fatal("existing capture backfilled round credit", err)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
}
