package backup

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

func roundAdoptionInventoryFixture(t *testing.T, replace bool) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "adoption-cli", Provider: "coderabbit", Account: "adoption-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err = e.Configure(t.Context(), scope, "adoption-queue", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "adoption-cli", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	p := roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 4, Channel: "cli", Source: "original-source", SourceKey: "original-rule", SourceSHA256: strings.Repeat("a", 64), MaximumRemaining: 4, InitialDry: 2, Taper: 3}
	preview, err := e.PreviewRoundAdoption(t.Context(), scope, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ApplyRoundAdoption(t.Context(), scope, "adopt", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(t.Context(), scope, "new-native-round", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("invalid operation")
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
	ref, err := captures.Save(t.Context(), a.ID, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Started: true, Stdout: []byte("synthetic adopted native source")}, StartedAt: at, FinishedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := e.RecordReviewEvidence(t.Context(), f.project, a.ID, reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{"example.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "captured_native_result", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = e.FinalizeReviewEvidence(t.Context(), scope, "finalize-native", evidence.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
		t.Fatal(err)
	}
	c, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil || c.Used != 1 || c.InitialDry != 2 || c.Dry != 3 || !c.Tapered {
		t.Fatal("real native accounting missing", c, err)
	}
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "close-carried", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	if replace {
		two, min, max := int64(2), int64(1), int64(2)
		if _, err = e.ConfigureRoundControl(t.Context(), scope, "replace", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &two, Action: "grant", Minimum: &min, Maximum: &max}); err != nil {
			t.Fatal(err)
		}
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestRoundAdoptionInventoryRoundTrip(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed-carried", true: "replacement-grant"}[replace], func(t *testing.T) {
			f := roundAdoptionInventoryFixture(t, replace)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
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
			if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			after := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && after[table] != rows {
					t.Fatal("restore changed retained table", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("roundtrip mutated source/bundle")
			}
		})
	}
}
func TestRoundAdoptionInventoryResealedCorruptionRefused(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing-provenance", `DELETE FROM review_round_adoptions`},
		{"wrong-prepared-seed", `UPDATE review_round_adoptions SET prepared_json=json_set(prepared_json,'$.initial_dry',1)`},
		{"wrong-original-source", `UPDATE review_round_adoptions SET source='other'`},
		{"blob-key", `UPDATE review_round_adoptions SET source_key=CAST(source_key AS BLOB)`},
		{"false-initial-credit", `UPDATE review_round_controls SET initial_dry=3`},
		{"missing-real-completion", `DELETE FROM review_round_completions`},
		{"unexpected-index", `CREATE INDEX arbitrary_round_adoption ON review_round_adoptions(source)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := roundAdoptionInventoryFixture(t, false)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("refusal changed original state")
			}
		})
	}
}
func TestRoundAdoptionInventoryVersionOnePredecessor(t *testing.T) {
	f := newRestoreFixture(t)
	for _, query := range []string{`DROP TABLE review_round_adoptions`, `DELETE FROM metadata WHERE key='schema:review_round_adoption'`, `DROP TABLE review_round_controls`, reviewexec.RoundSchemaV1SQL()[0], `UPDATE metadata SET value='1' WHERE key='schema:review_rounds'`} {
		if _, err := f.store.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("genuine old round schema refused", err)
	}
}

func TestRoundAdoptionInventoryStickyMinimumBaselineRestored(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "minimum-cli", Provider: "coderabbit", Account: "minimum-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err = e.Configure(t.Context(), scope, "minimum-queue", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "minimum-cli", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	p := roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 4, Channel: "cli", Source: "minimum-source", SourceKey: "minimum-rule", SourceSHA256: strings.Repeat("a", 64), MinimumRemaining: 2, MaximumRemaining: 4, Taper: 3, Tapered: true}
	preview, err := e.PreviewRoundAdoption(t.Context(), scope, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ApplyRoundAdoption(t.Context(), scope, "sticky-minimum", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}); err != nil {
		t.Fatal(err)
	}
	original, err := e.ReadRoundControl(t.Context(), scope, 4, "cli")
	if err != nil || !original.InitiallyTapered || original.Tapered || original.Used != 0 {
		t.Fatal("minimum baseline wrong", original, err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err = Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	var dry, used, initial, latch, tapered int64
	if err = db.QueryRow(`SELECT dry,used,initial_dry,initially_tapered,tapered FROM review_round_controls`).Scan(&dry, &used, &initial, &latch, &tapered); err != nil || dry != 0 || used != 0 || initial != 0 || latch != 1 || tapered != 0 {
		t.Fatal("restored baseline lost independent latch/minimum", err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("restore changed source")
	}
}
