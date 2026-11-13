package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Genuine finalized native evidence supplies the original authority. Only the
// trusted Git attestation is synthetic; backup inspection must never rerun Git.
func applicabilityInventoryFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := comparisonInventoryFixture(t, true)
	var body string
	if err := f.store.DB().QueryRow(`SELECT e.body_json FROM review_execution_evidence e JOIN review_operations o ON o.id=e.operation_id WHERE o.channel='cli'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var evidence reviewexec.ReviewEvidence
	if err := json.Unmarshal([]byte(body), &evidence); err != nil {
		t.Fatal(err)
	}
	current := evidence.Anchor
	current.CandidateSHA, current.BaseSHA = strings.Repeat("d", 40), strings.Repeat("e", 40)
	current.ParentSHA, current.MergeBaseSHA = current.BaseSHA, current.BaseSHA
	p := reviewexec.ReviewApplicability{
		EvidenceID: evidence.ID, Channel: "cli", Original: evidence.Anchor, Current: current,
		Trees: reviewexec.EquivalentComparison{
			OriginalBaseTree: strings.Repeat("1", 40), CurrentBaseTree: strings.Repeat("1", 40),
			OriginalHeadTree: strings.Repeat("2", 40), CurrentHeadTree: strings.Repeat("2", 40),
			OriginalMergeBaseSHA: evidence.Anchor.MergeBaseSHA, CurrentMergeBaseSHA: current.MergeBaseSHA,
			PatchSHA256: evidence.Anchor.PatchSHA256, InventorySHA256: strings.Repeat("3", 64),
		},
		RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	stable := p
	stable.Original.ObservedAt, stable.Current.ObservedAt, stable.RecordedAt = "", "", ""
	identity, err := json.Marshal(stable)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(identity)
	id := "review_applicability_" + hex.EncodeToString(hash[:16])
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`INSERT INTO review_applicability(id,project_id,evidence_id,repository_id,pull_number,channel,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,body_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, f.project, p.EvidenceID, current.RepositoryID, current.PullNumber, p.Channel, current.CandidateSHA, current.BaseSHA, current.ParentSHA, current.ParentPullNumber, current.BaseRef, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err = reviewexec.ValidateReviewApplicability(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine evidence-backed applicability fixture invalid", err)
	}
	return f
}

func TestApplicabilityInventoryRoundTripPreservesImmutableAuthority(t *testing.T) {
	f := applicabilityInventoryFixture(t)
	before := backupRows(t, f.store.DB())
	for _, statement := range []string{`UPDATE review_applicability SET base_ref='other'`, `DELETE FROM review_applicability`} {
		if _, err := f.store.DB().Exec(statement); err == nil {
			t.Fatal("applicability retention/immutability guard did not reject mutation")
		}
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-applicability")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = reviewexec.ValidateReviewApplicability(t.Context(), db); err != nil {
		t.Fatal("inactive restore invalidated retained applicability", err)
	}
	retained := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && retained[table] != rows {
			t.Fatalf("restore changed applicability, native evidence or history: %s", table)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup inspection or restore changed source or immutable bundle")
	}
}

func TestApplicabilityInventoryRejectsResealedRows(t *testing.T) {
	var immutable string
	for _, statement := range reviewexec.ApplicabilitySchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_applicability_immutable ") {
			immutable = statement
		}
	}
	if immutable == "" {
		t.Fatal("missing canonical immutability trigger")
	}
	for _, tc := range []struct{ name, sql string }{
		{"empty-identity", `UPDATE review_applicability SET id=''`},
		{"wrong-identity", `UPDATE review_applicability SET id='review_applicability_forged'`},
		{"malformed-body", `UPDATE review_applicability SET body_json='null'`},
		{"unknown-body-field", `UPDATE review_applicability SET body_json=substr(body_json,1,length(body_json)-1)||',"unknown":true}'`},
		{"duplicate-body-field", `UPDATE review_applicability SET body_json=replace(body_json,'"evidence_id":','"evidence_id":"forged","evidence_id":')`},
		{"wrong-channel", `UPDATE review_applicability SET channel='hosted'`},
		{"wrong-target", `UPDATE review_applicability SET candidate_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`},
		{"wrong-repository", `UPDATE review_applicability SET repository_id=992`},
		{"wrong-pull", `UPDATE review_applicability SET pull_number=pull_number+1`},
		{"wrong-parent", `UPDATE review_applicability SET parent_pull_number=parent_pull_number+1`},
		{"wrong-ref", `UPDATE review_applicability SET base_ref='other'`},
		{"wrong-original", `UPDATE review_applicability SET body_json=replace(body_json,'synthetic/restore','synthetic/other')`},
		{"wrong-tree", `UPDATE review_applicability SET body_json=replace(body_json,'"CurrentHeadTree":"2222222222222222222222222222222222222222"','"CurrentHeadTree":"4444444444444444444444444444444444444444"')`},
		{"wrong-inventory", `UPDATE review_applicability SET body_json=replace(body_json,'3333333333333333333333333333333333333333333333333333333333333333','not-a-digest')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := applicabilityInventoryFixture(t)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `DROP TRIGGER review_applicability_immutable; `+tc.sql+`; `+immutable)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("malformed bundle changed source applicability authority")
			}
		})
	}
}

func TestApplicabilityInventoryRequiresCanonicalSchemaAndGuards(t *testing.T) {
	tests := []struct{ name, sql string }{
		{"missing-table", `DROP TABLE review_applicability`},
		{"missing-domain", `DELETE FROM metadata WHERE key='schema:review_applicability'`},
		{"wrong-domain-version", `UPDATE metadata SET value='2' WHERE key='schema:review_applicability'`},
		{"missing-index", `DROP INDEX review_applicability_target`},
		{"wrong-index", `DROP INDEX review_applicability_target; CREATE INDEX review_applicability_target ON review_applicability(evidence_id)`},
		{"extra-index", `CREATE INDEX invented_applicability_index ON review_applicability(evidence_id)`},
		{"missing-retain-trigger", `DROP TRIGGER review_applicability_retain`},
		{"missing-immutable-trigger", `DROP TRIGGER review_applicability_immutable`},
		{"wrong-trigger", `DROP TRIGGER review_applicability_immutable; CREATE TRIGGER review_applicability_immutable BEFORE UPDATE ON review_applicability BEGIN SELECT 1; END`},
		{"extra-trigger", `CREATE TRIGGER invented_applicability BEFORE INSERT ON review_applicability BEGIN SELECT 1; END`},
		{"extra-column", `ALTER TABLE review_applicability ADD COLUMN invented_credit INTEGER`},
	}
	for _, change := range []struct{ name, old, replacement string }{
		{"nullable-key", "id TEXT PRIMARY KEY NOT NULL", "id TEXT PRIMARY KEY"},
		{"wrong-key", "id TEXT PRIMARY KEY NOT NULL", "id TEXT UNIQUE NOT NULL"},
		{"wrong-type", "repository_id INTEGER", "repository_id TEXT"},
		{"wrong-foreign-key", "REFERENCES projects(id)", "REFERENCES projects(id) ON DELETE CASCADE"},
	} {
		definitions := reviewexec.ApplicabilitySchemaSQL()
		changed := strings.Replace(definitions[0], change.old, change.replacement, 1)
		if changed == definitions[0] {
			t.Fatal("canonical applicability schema changed", change.name)
		}
		definitions[0] = changed
		tests = append(tests, struct{ name, sql string }{change.name, `DROP TABLE review_applicability; ` + strings.Join(definitions, "\n")})
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

func TestApplicabilityInventorySupportsOlderBundleWithoutDomain(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := f.store.DB().Exec(`DROP TABLE review_applicability; DELETE FROM metadata WHERE key='schema:review_applicability'`); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("supported predecessor without applicability refused", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-predecessor")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var added int
	if err = db.QueryRow(`SELECT count(*) FROM metadata WHERE key='schema:review_applicability'`).Scan(&added); err != nil || added != 0 {
		t.Fatal("restore installed optional applicability schema", added, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("predecessor backup operations changed source or bundle")
	}
}

func TestApplicabilityInventoryCancellationIsUnavailableAndReadOnly(t *testing.T) {
	f := applicabilityInventoryFixture(t)
	rows := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	for _, operation := range []string{"inspect", "restore"} {
		t.Run(operation, func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &historyScanCancellation{Context: base, cancel: cancel, phase: "github.com/benhook1013/FireController/internal/reviewexec.ValidateReviewApplicability"}
			target := filepath.Join(t.TempDir(), "must-not-create")
			var err error
			if operation == "inspect" {
				_, err = Inspect(ctx, bundle.Path)
			} else {
				_, err = Restore(ctx, bundle.Path, target)
			}
			var failure *core.Error
			if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) || !errors.As(err, &failure) || failure.Code != "backup_unavailable" || failure.Status != 503 {
				t.Fatal("applicability interruption lost retryable classification", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("interrupted validation created a restore destination", err)
			}
			if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("interrupted validation changed source or immutable bundle")
			}
		})
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("transient interruption was retained as corruption", err)
	}
}
