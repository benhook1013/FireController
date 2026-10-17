package backup

import (
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Complete actual locally captured native work; imported dashboard records in
// the existing fixture cannot create an operation proof or supply its credit.
func comparisonInventoryFixture(t *testing.T, finalized bool) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	bundleNativeReferences(t, f)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	var id, evidence string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT a.id,a.revision,e.id FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id JOIN review_execution_evidence e ON e.attempt_id=a.id WHERE o.channel='cli'`).Scan(&id, &revision, &evidence); err != nil {
		t.Fatal(err)
	}
	a, err := e.Advance(t.Context(), f.project, id, revision, "completed", "fixture-captured", "")
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("project_id", f.project)
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("X-FC-Worker", f.worker)
		scope, err := f.store.AdminScope(r)
		if err != nil {
			t.Fatal(err)
		}
		zero := int64(0)
		if _, err := e.FinalizeReviewEvidence(t.Context(), scope, "inventory-finalize", evidence, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	anchor := a.Anchor
	plan := reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: anchor.ObservedAt}
	got, err := e.Comparison(t.Context(), f.project, plan, "cli")
	if err != nil || got != (reviewexec.ComparisonState{Covered: finalized, Pending: !finalized}) {
		t.Fatal("genuine native comparison fixture", got, err)
	}
	if err := reviewexec.ValidateComparisonProofs(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine completed proof failed validation", err)
	}
	return f
}

func TestComparisonInventoryCapturedReadyRoundTripIsReadOnly(t *testing.T) {
	f := comparisonInventoryFixture(t, true)
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-native-proof")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := reviewexec.ValidateComparisonProofs(t.Context(), db); err != nil {
		t.Fatal("restore invalidated historic native readiness", err)
	}
	retained := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && retained[table] != rows {
			t.Fatalf("restore changed native proof authority or retained records: %s", table)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("create / inspect / restore changed source or immutable bundle")
	}
}

func TestComparisonInventoryRehashedCanonicalProofDamageRefused(t *testing.T) {
	var retain string
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_proofs_retain ") {
			retain = statement
		}
	}
	if retain == "" {
		t.Fatal("canonical retention guard missing")
	}
	for _, tc := range []struct {
		name, sql string
		pending   bool
	}{
		{"forged-ready", `UPDATE review_comparison_proofs SET state='ready'`, true},
		{"forged-pending", `UPDATE review_comparison_proofs SET state='pending'`, false},
		{"dirty-authority", `UPDATE review_operations SET reason='direct-authority-edit' WHERE channel='cli'`, false},
		{"wrong-pull", `UPDATE review_comparison_proofs SET pull_number=pull_number+1`, false},
		{"wrong-channel", `UPDATE review_comparison_proofs SET channel='hosted'`, false},
		{"wrong-anchor", `UPDATE review_comparison_proofs SET candidate_sha=replace(candidate_sha,'a','9')`, false},
		{"wrong-evidence", `UPDATE review_comparison_proofs SET evidence_id='review_evidence_forged'`, false},
		{"wrong-project", `INSERT INTO projects VALUES('unrelated-project','Other',992,'synthetic/other'); UPDATE review_comparison_proofs SET project_id='unrelated-project'`, false},
		{"missing-proof", `DROP TRIGGER review_comparison_proofs_retain; DELETE FROM review_comparison_proofs; ` + retain, false},
		{"missing-native-capture", `DELETE FROM review_adjudication_changes; DELETE FROM review_adjudication_state; DELETE FROM review_execution_evidence; UPDATE review_comparison_proofs SET state='ready'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := comparisonInventoryFixture(t, !tc.pending)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			// Reuses the existing strict negative boundary: exact schema and
			// ordinary FKs must still pass, Inspect/Restore must refuse before
			// destination creation, and the rejected bundle stays byte-identical.
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("damaged bundle changed source authority")
			}
		})
	}
}

func TestComparisonInventoryCanonicalSchemaAndInvalidationTriggersRequired(t *testing.T) {
	tests := []struct{ name, sql string }{
		{"missing-index", `DROP INDEX review_comparison_proofs_anchor`},
		{"wrong-index", `DROP INDEX review_comparison_proofs_state; CREATE INDEX review_comparison_proofs_state ON review_comparison_proofs(operation_id)`},
		{"unknown-index", `CREATE INDEX extra_comparison_index ON review_comparison_proofs(evidence_id)`},
		{"missing-trigger", `DROP TRIGGER review_comparison_dirty_review_execution_evidence_update`},
		{"wrong-trigger", `DROP TRIGGER review_comparison_repository_changed; CREATE TRIGGER review_comparison_repository_changed AFTER UPDATE ON projects BEGIN SELECT 1; END`},
		{"unknown-trigger", `CREATE TRIGGER unexpected_comparison BEFORE INSERT ON review_comparison_proofs BEGIN SELECT 1; END`},
		{"extra-column", `ALTER TABLE review_comparison_proofs ADD COLUMN fabricated_credit INTEGER`},
		{"wrong-domain-version", `UPDATE metadata SET value='2' WHERE key='schema:review_comparison'`},
	}
	var table, retain string
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TABLE review_comparison_proofs ") {
			table = statement
		}
		if strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_proofs_retain ") {
			retain = statement
		}
	}
	for _, change := range []struct{ name, old, replacement string }{
		{"changed-primary-key", "operation_id TEXT NOT NULL PRIMARY KEY", "operation_id TEXT NOT NULL UNIQUE"},
		{"changed-foreign-key", "REFERENCES review_operations(id)", "REFERENCES review_operations(id) ON DELETE CASCADE"},
		{"changed-column-type", "repository_id INTEGER", "repository_id TEXT"},
		{"changed-default", "repository_id INTEGER NOT NULL DEFAULT 0", "repository_id INTEGER NOT NULL DEFAULT 1"},
	} {
		changed := strings.Replace(table, change.old, change.replacement, 1)
		if changed == table || table == "" || retain == "" {
			t.Fatal("missing canonical schema fixture", change.name)
		}
		tests = append(tests, struct{ name, sql string }{change.name, `DROP TABLE review_comparison_proofs; ` + changed + retain})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
}

func TestComparisonInventoryNullOperationIdentityRefused(t *testing.T) {
	f := newRestoreFixture(t)
	before := backupRows(t, f.store.DB())
	if _, err := f.store.DB().Exec(`INSERT INTO review_comparison_proofs(operation_id,project_id,pull_number,channel,state) VALUES(NULL,?,2,'cli','dirty')`, f.project); err == nil {
		t.Fatal("canonical proof primary key accepted NULL")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("failed NULL insertion changed canonical state")
	}
	bundle := restoreBundle(t, f)
	statements := []string{`DROP TABLE review_comparison_proofs;`}
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TABLE review_comparison_proofs ") {
			changed := strings.Replace(statement, "operation_id TEXT NOT NULL PRIMARY KEY", "operation_id TEXT PRIMARY KEY", 1)
			if changed == statement {
				t.Fatal("missing canonical NOT NULL primary key")
			}
			statements = append(statements, changed)
		} else if strings.HasPrefix(statement, "CREATE INDEX ") || strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_proofs_retain ") {
			statements = append(statements, statement)
		}
	}
	statements = append(statements, `INSERT INTO review_comparison_proofs(operation_id,project_id,pull_number,channel,state) VALUES(NULL,?,2,'cli','dirty')`)
	mutateBundleSQL(t, bundle.Path, strings.Join(statements, "\n"), f.project)
	db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = reviewexec.ValidateComparisonProofs(t.Context(), db); err == nil {
		db.Close()
		t.Fatal("maintenance validation skipped NULL operation identity")
	}
	if _, err = validateSchema(t.Context(), db, false); err == nil {
		db.Close()
		t.Fatal("exact inventory accepted weakened nullable primary key")
	}
	db.Close()
	assertCurrentSummaryInventoryRefused(t, bundle, false)
}

func TestComparisonInventoryEmptyNativeOperationIdentityRefusedBeforePaging(t *testing.T) {
	f := comparisonInventoryFixture(t, true)
	bundle := restoreBundle(t, f)
	var operation string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_operations WHERE channel='cli' AND state='completed'`).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	var retain string
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_proofs_retain ") {
			retain = statement
		}
	}
	if retain == "" {
		t.Fatal("missing canonical retention trigger")
	}
	// Retag this genuine native operation and its exact capture/attempt linkage
	// in the disposable bundle. All ordinary FKs and canonical schema remain
	// valid; the original capture fingerprint is unchanged. Invalidation leaves
	// the empty-identity proof dirty, never fabricates ready credit. A keyset
	// starting at '' must not silently skip this malformed journal identity.
	mutateBundleSQL(t, bundle.Path, `DROP TRIGGER review_comparison_proofs_retain;
 UPDATE review_operations SET id='' WHERE id=?;
 UPDATE review_execution_attempts SET operation_id='' WHERE operation_id=?;
 UPDATE review_execution_evidence SET operation_id='',body_json=replace(body_json,?,'') WHERE operation_id=?;
 DELETE FROM review_comparison_proofs WHERE operation_id=?;
 `+retain, operation, operation, operation, operation, operation)
	db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM review_comparison_proofs WHERE operation_id=''`).Scan(&state); err != nil || state != "dirty" {
		db.Close()
		t.Fatal("fixture did not preserve invalidated native proof", state, err)
	}
	before := backupRows(t, db)
	if err := reviewexec.ValidateComparisonProofs(t.Context(), db); err == nil {
		db.Close()
		t.Fatal("maintenance validator skipped empty native operation identity")
	}
	if !reflect.DeepEqual(before, backupRows(t, db)) {
		db.Close()
		t.Fatal("maintenance validation rewrote malformed native authority")
	}
	db.Close()
	assertCurrentSummaryInventoryRefused(t, bundle, true)
}

func TestComparisonInventoryImportedHistoryCreatesNoNativeProof(t *testing.T) {
	f := newRestoreFixture(t)
	var proofs, imported int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_comparison_proofs`).Scan(&proofs); err != nil || proofs != 0 {
		t.Fatal("imported completed / finalized dashboard history created native proof", proofs, err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_run_versions WHERE finalized=1`).Scan(&imported); err != nil || imported == 0 {
		t.Fatal("fixture lacks retained imported finalized history", imported, err)
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("canonical imported history was incorrectly refused", err)
	}
}

func TestComparisonInventoryAbsentOptionalDomainRemainsSupported(t *testing.T) {
	store, dir := historyInventoryFixture(t)
	before := backupRows(t, store.DB())
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "predecessor-bundles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("supported predecessor without comparison domain refused", err)
	}
	destination := filepath.Join(t.TempDir(), "predecessor-inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var added int
	if err := db.QueryRow(`SELECT count(*) FROM metadata WHERE key='schema:review_comparison'`).Scan(&added); err != nil || added != 0 {
		t.Fatal("restore migrated predecessor comparison schema", added, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
		t.Fatal("predecessor inspection / restore changed source")
	}
}
