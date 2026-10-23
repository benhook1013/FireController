package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

type adoptionBackupFixture struct {
	*restoreFixture
	engine   *reviewexec.Engine
	service  *adoptionholds.Service
	scope    core.Scope
	source   m.Source
	prepared roundadoptionmodel.Prepared
}

func adoptionNativeRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	all := backupRows(t, db)
	out := map[string]string{}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions", "review_round_operation_bindings", "review_execution_reservations"} {
		out[table] = all[table]
	}
	return out
}
func applyBackupObligations(t *testing.T, f *adoptionBackupFixture, b m.Batch, key string) {
	t.Helper()
	before := backupRows(t, f.store.DB())
	preview, err := f.service.Preview(t.Context(), f.scope, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("obligation preview wrote state")
	}
	if _, err = f.service.Apply(t.Context(), f.scope, key, m.ApplyRequest{Batch: b, PreviewSHA256: preview.PreviewSHA256}); err != nil {
		t.Fatal(err)
	}
}
func newAdoptionBackupFixture(t *testing.T) *adoptionBackupFixture {
	t.Helper()
	base := newRestoreFixture(t)
	if err := adoptionholds.EnsureSchema(t.Context(), base.store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(base.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "backup-unbounded", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &adoptionBackupFixture{restoreFixture: base, engine: engine, service: adoptionholds.New(base.store), scope: routePolicyInventoryScope(t, base), source: m.Source{Namespace: "sealed-source", RepositoryID: 991, SnapshotSHA256: strings.Repeat("b", 64)}}
	native := adoptionNativeRows(t, base.store.DB())
	one, zero := int64(1), int64(0)
	batch := m.Batch{Version: 1, Source: f.source, ExpectedRevision: &zero, Obligations: []m.Obligation{{SourceKey: "finding-one", Kind: "finding", AffectedPullNumbers: []int64{4}, Summary: "Original pending finding", Evidence: []string{"source://finding-one"}, OriginalStatus: "pending"}, {SourceKey: "route-one", Kind: "route", AffectedPullNumbers: []int64{4, 5}, Summary: "Original unresolved source route", Evidence: []string{}, OriginalStatus: "open"}}}
	applyBackupObligations(t, f, batch, "backup-source-one")
	page, err := f.service.List(t.Context(), f.scope, 4, "open", 10, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatal("missing actual source holds", page, err)
	}
	var resolved string
	for _, item := range page.Items {
		if item.SourceKey == "finding-one" {
			resolved = item.ID
		}
	}
	if resolved == "" {
		t.Fatal("finding identity missing")
	}
	if _, err = f.service.Resolve(t.Context(), f.scope, resolved, "backup-source-resolution", m.ResolveRequest{ExpectedRevision: &one, Reason: "Original correction verified independently.", Evidence: []string{"source://verified-fix"}}); err != nil {
		t.Fatal(err)
	}
	f.prepared = roundadoptionmodel.Prepared{Version: 2, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 4, Channel: "cli", Source: f.source.Namespace, SourceKey: "minimum-only-rule", SourceSHA256: strings.Repeat("a", 64), MinimumRemaining: 1001, MaximumRemaining: 0, MaximumUnbounded: true, InitialDry: 2, Taper: 3, Obligations: &roundadoptionmodel.ObligationsReference{Namespace: f.source.Namespace, SnapshotSHA256: f.source.SnapshotSHA256, Revision: 1}}
	preview, err := engine.PreviewRoundAdoption(t.Context(), f.scope, f.prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.ApplyRoundAdoption(t.Context(), f.scope, "backup-linked-policy", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: f.prepared}); err != nil {
		t.Fatal(err)
	}
	// Imported PR4 controls precede enrollment; new PR5/7 still receive defaults.
	if _, err = engine.Configure(t.Context(), f.scope, "backup-unbounded-queue", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "backup-unbounded", Mode: "independent", Queue: []int64{4, 5, 7}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.ConfigureRoundControl(t.Context(), f.scope, "backup-close-policy", 4, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	// A real additive import advances current source revision after policy bind.
	// Retained policy must continue to validate against its immutable revision 1.
	batch.ExpectedRevision = &one
	batch.Obligations = []m.Obligation{{SourceKey: "later-finding", Kind: "finding", AffectedPullNumbers: []int64{7}, Summary: "Later additive obligation", Evidence: []string{"source://later"}, OriginalStatus: "open"}}
	applyBackupObligations(t, f, batch, "backup-source-two")
	stale := f.prepared
	// Preview a future, unconfigured target; enrolled PR5/7 have native defaults.
	stale.PullNumber = 8
	stale.SourceKey = "stale-next-policy"
	if _, err = engine.PreviewRoundAdoption(t.Context(), f.scope, stale); err == nil {
		t.Fatal("new policy admitted against stale current source revision")
	}
	ref := *stale.Obligations
	ref.Revision = 2
	stale.Obligations = &ref
	if _, err = engine.PreviewRoundAdoption(t.Context(), f.scope, stale); err != nil {
		t.Fatal("current source binding refused", err)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("source advancement invalidated retained bound policy", err)
	}
	if err = adoptionholds.ValidateSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(native, adoptionNativeRows(t, base.store.DB())) {
		t.Fatal("source obligation/policy setup fabricated native work or completion credit")
	}
	return f
}
func TestAdoptionObligationsCanonicalBackupRestoreBoundHistory(t *testing.T) {
	f := newAdoptionBackupFixture(t)
	before := backupRows(t, f.store.DB())
	native := adoptionNativeRows(t, f.store.DB())
	bundle := restoreBundle(t, f.restoreFixture)
	sealed := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	summary, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != "inactive" || summary.SourceInstanceID != f.store.InstanceID() || summary.InstanceID == summary.SourceInstanceID {
		t.Fatal("restore failed to rotate authority", summary)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = adoptionholds.ValidateSnapshot(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
		t.Fatal("retained old policy/source revision binding lost", err)
	}
	after := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatal("restored history changed", table)
		}
	}
	if !reflect.DeepEqual(native, adoptionNativeRows(t, db)) {
		t.Fatal("restore manufactured native credit")
	}
	var open, resolved, imports int
	if err = db.QueryRow(`SELECT (SELECT count(*) FROM review_adoption_obligations WHERE status='open'),(SELECT count(*) FROM review_adoption_obligations WHERE status='resolved'),(SELECT count(*) FROM review_adoption_imports)`).Scan(&open, &resolved, &imports); err != nil || open != 2 || resolved != 1 || imports != 2 {
		t.Fatal("lost unresolved/resolved/import history", open, resolved, imports, err)
	}
	facts, err := adoptionholds.ReadFacts(t.Context(), db, f.project, 4)
	if err != nil || facts.Open != 1 {
		t.Fatal("restored route no longer holds all affected work", facts, err)
	}
	facts, err = adoptionholds.ReadFacts(t.Context(), db, f.project, 5)
	if err != nil || facts.Open != 1 {
		t.Fatal("restored route lost receiving target", facts, err)
	}
	oldSource := restoreRows(t, f.store.DB(), `SELECT value FROM metadata WHERE key='signing_key'`)[0][0]
	newSource := restoreRows(t, db, `SELECT value FROM metadata WHERE key='signing_key'`)[0][0]
	if oldSource == newSource {
		t.Fatal("signing authority not rotated")
	}
	if restoreRows(t, db, `SELECT count(*) FROM credentials WHERE revoked<>1`)[0][0] != int64(0) {
		t.Fatal("old credentials retained active authority")
	}
	// The inactive restore remains providers-held before any engine is created.
	recovery, err := readRecoveryStatus(t.Context(), db)
	if err != nil || recovery.State != "inactive" || !recovery.ProvidersHeld {
		t.Fatal("providers were not held", recovery, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(sealed, restoreTree(t, bundle.Path)) {
		t.Fatal("backup/restore changed source or sealed bundle")
	}
	restored, err := core.Open(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoreCode(t, StartupGuard(context.Background(), restored), "restore_inactive")
}
func adoptionTrigger(t *testing.T, name string) string {
	t.Helper()
	for _, statement := range adoptionholds.SchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER "+name+" ") {
			return statement
		}
	}
	t.Fatal("canonical trigger unavailable", name)
	return ""
}
func TestAdoptionObligationsResealedCorruptionRefusedBeforeRestore(t *testing.T) {
	tests := []struct {
		name, sql string
		physical  bool
	}{
		{"policy-snapshot-binding", `UPDATE review_round_adoptions SET prepared_json=json_set(prepared_json,'$.obligations.snapshot_sha256','` + strings.Repeat("a", 64) + `')`, true},
		{"policy-future-source-revision", `UPDATE review_round_adoptions SET prepared_json=json_set(prepared_json,'$.obligations.revision',3)`, true},
		{"unexpected-route-target", `INSERT INTO review_adoption_targets SELECT id,project_id,6 FROM review_adoption_obligations WHERE source_key='route-one'`, true},
		{"missing-resolution", `DROP TRIGGER review_adoption_resolutions_retain; DELETE FROM review_adoption_resolutions; ` + adoptionTrigger(t, "review_adoption_resolutions_retain"), true},
		{"empty-resolution-evidence", `DROP TRIGGER review_adoption_resolutions_immutable; UPDATE review_adoption_resolutions SET evidence_json='[]'; ` + adoptionTrigger(t, "review_adoption_resolutions_immutable"), true},
		{"missing-original", `DROP TRIGGER review_adoption_targets_retain; DROP TRIGGER review_adoption_obligations_retain; DELETE FROM review_adoption_targets WHERE obligation_id IN (SELECT id FROM review_adoption_obligations WHERE source_key='later-finding'); DELETE FROM review_adoption_obligations WHERE source_key='later-finding'; ` + adoptionTrigger(t, "review_adoption_targets_retain") + adoptionTrigger(t, "review_adoption_obligations_retain"), true},
		{"missing-policy-provenance", `DELETE FROM review_round_adoptions`, true},
		{"missing-immutable-trigger", `DROP TRIGGER review_adoption_originals_immutable`, false},
		{"extra-index", `CREATE INDEX arbitrary_source_scope ON review_adoption_obligations(namespace)`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptionBackupFixture(t)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f.restoreFixture)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, tc.physical)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("refusal altered source")
			}
		})
	}
}
