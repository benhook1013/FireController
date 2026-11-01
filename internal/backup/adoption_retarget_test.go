package backup

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
)

func TestAdoptionRetargetCanonicalExportInspectInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	service := adoptionholds.New(f.store)
	scope := routePolicyInventoryScope(t, f)
	zero := int64(0)
	batch := m.Batch{Version: 1, Source: m.Source{Namespace: "retained-route-source", RepositoryID: 991, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []m.Obligation{
		{SourceKey: "current-route", Kind: "route", AffectedPullNumbers: []int64{2, 4}, Summary: "Retained correction requiring a new receiver", Evidence: []string{"target-pr:4"}, OriginalStatus: "open"},
		{SourceKey: "resolved-route", Kind: "route", AffectedPullNumbers: []int64{8}, Summary: "Retained correction later resolved", Evidence: []string{"target-pr:unassigned"}, OriginalStatus: "open"},
	}}
	preview, err := service.Preview(t.Context(), scope, batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(t.Context(), scope, "adopt-routes-for-retarget-backup", m.ApplyRequest{Batch: batch, PreviewSHA256: preview.PreviewSHA256}); err != nil {
		t.Fatal(err)
	}
	var openID, resolvedID, originalOpen, originalResolved string
	if err := f.store.DB().QueryRow(`SELECT id,original_json FROM review_adoption_obligations WHERE source_key='current-route'`).Scan(&openID, &originalOpen); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT id,original_json FROM review_adoption_obligations WHERE source_key='resolved-route'`).Scan(&resolvedID, &originalResolved); err != nil {
		t.Fatal(err)
	}
	native := adoptionNativeRows(t, f.store.DB())
	beforeRetarget := backupRows(t, f.store.DB())
	var firstReceipt json.RawMessage
	for index, target := range []int64{6, 0, 7} {
		revision := int64(index + 1)
		receipt, err := service.Retarget(t.Context(), scope, openID, "retarget-open-"+string(rune('0'+index)), m.RetargetRequest{ExpectedRevision: &revision, TargetPullNumber: &target, Reason: "Assign the retained correction to its current receiver."})
		if err != nil {
			t.Fatal("retarget retained open route", index, err)
		}
		if index == 0 {
			firstReceipt = receipt
		}
	}
	one, target := int64(1), int64(9)
	if _, err := service.Retarget(t.Context(), scope, resolvedID, "retarget-before-resolution", m.RetargetRequest{ExpectedRevision: &one, TargetPullNumber: &target, Reason: "Move this correction to its implementing PR."}); err != nil {
		t.Fatal(err)
	}
	two := int64(2)
	if _, err := service.Resolve(t.Context(), scope, resolvedID, "resolve-retargeted-route", m.ResolveRequest{ExpectedRevision: &two, Reason: "Correction verified in the receiving PR.", Evidence: []string{"proof:receiving-fix"}}); err != nil {
		t.Fatal(err)
	}
	// Receipt replay is historical recovery, not a new routing instruction.
	target = 6
	replayed, err := service.Retarget(t.Context(), scope, openID, "retarget-open-0", m.RetargetRequest{ExpectedRevision: &one, TargetPullNumber: &target, Reason: "Assign the retained correction to its current receiver."})
	if err != nil || string(firstReceipt) != string(replayed) {
		t.Fatal("original retarget key did not recover its original receipt", err)
	}
	if !reflect.DeepEqual(native, adoptionNativeRows(t, f.store.DB())) {
		t.Fatal("retarget manufactured native routes, attempts or review credit")
	}
	before := backupRows(t, f.store.DB())
	for _, table := range []string{"review_finding_routes", "review_round_controls"} {
		if beforeRetarget[table] != before[table] {
			t.Fatal("retarget changed native routes or configured review progress", table)
		}
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("independent retarget bundle inspection", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-retargeted-routes")
	result, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || result.State != "inactive" {
		t.Fatal("inactive retarget restore", result, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_adoption_obligations", "review_adoption_targets", "review_adoption_retargets", "review_adoption_resolutions", "review_adoption_sources", "review_adoption_imports", "review_round_controls", "review_round_completions", "review_finding_routes"} {
		if after[table] != before[table] {
			t.Fatal("retarget restore changed retained history/current scope/accounting", table)
		}
	}
	current, err := adoptionholds.ReadDetail(t.Context(), db, f.project, openID)
	if err != nil || current.Status != "open" || current.Revision != 4 || !reflect.DeepEqual(current.AffectedPullNumbers, []int64{2, 7}) || current.Retarget == nil || current.Retarget.TargetPullNumber != 7 {
		t.Fatal("restored open route lost its current receiver or preserved source scope", current, err)
	}
	resolved, err := adoptionholds.ReadDetail(t.Context(), db, f.project, resolvedID)
	if err != nil || resolved.Status != "resolved" || resolved.Revision != 3 || resolved.Resolution == nil || resolved.Resolution.Revision != 3 || resolved.Retarget == nil || resolved.Retarget.TargetPullNumber != 9 {
		t.Fatal("resolved retarget history failed after restore", resolved, err)
	}
	var restoredOpen, restoredResolved string
	if err := db.QueryRow(`SELECT original_json FROM review_adoption_obligations WHERE id=?`, openID).Scan(&restoredOpen); err != nil || restoredOpen != originalOpen {
		t.Fatal("open original provenance was rewritten", err)
	}
	if err := db.QueryRow(`SELECT original_json FROM review_adoption_obligations WHERE id=?`, resolvedID).Scan(&restoredResolved); err != nil || restoredResolved != originalResolved {
		t.Fatal("resolved original provenance was rewritten", err)
	}
	for pull, want := range map[int64]int64{2: 1, 4: 0, 6: 0, 7: 1, 8: 0, 9: 0} {
		facts, err := adoptionholds.ReadFacts(t.Context(), db, f.project, pull)
		if err != nil || facts.Open != want {
			t.Fatal("restored admission guard followed historical rather than current scope", pull, facts, err)
		}
	}
	if err := adoptionholds.ValidateSnapshot(t.Context(), db); err != nil {
		t.Fatal("restored retarget graph validation", err)
	}
	recovery, err := readRecoveryStatus(t.Context(), db)
	if err != nil || !recovery.ProvidersHeld || recovery.State != "inactive" {
		t.Fatal("retarget restore released provider work", recovery, err)
	}
	invalid := restoreModifiedBundle(t, bundle, `DROP TRIGGER review_adoption_targets_retain; DELETE FROM review_adoption_targets WHERE obligation_id='`+openID+`' AND pull_number=7; `+adoptionTrigger(t, "review_adoption_targets_retain"))
	if _, err := Inspect(t.Context(), invalid); err == nil {
		t.Fatal("rehashed bundle accepted a missing current receiver")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("retarget export/inspection/restore modified source or sealed bundle")
	}
}

func TestAdoptionRetargetInspectKeepsPriorVersionOneBackupsReadable(t *testing.T) {
	f := newRestoreFixture(t)
	// Reconstruct the empty predecessor domain only in this disposable fixture.
	// Inspect and Restore must read it directly rather than perform migration.
	for _, table := range []string{"review_adoption_retargets", "review_adoption_resolutions", "review_adoption_targets", "review_adoption_obligations", "review_adoption_imports", "review_adoption_sources"} {
		if _, err := f.store.DB().Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range adoptionholds.SchemaV1SQL() {
		if _, err := f.store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE metadata SET value='1' WHERE key='schema:review_adoption_obligations'`); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("prior version-one backup inspection", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-prior-adoption-domain")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal("prior version-one inactive restore", err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("backup read unexpectedly migrated its input", version, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("prior backup inspection changed source")
	}
}
