package backup

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

func routePolicyInventoryScope(t *testing.T, f *restoreFixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", f.worker)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

// Routes have native capture authority but need not wait for operation
// completion. This exercises receiving policy independently of comparison
// proofs and imported dashboard rows, using actual guarded route mutations.
func routePolicyInventoryFixture(t *testing.T, resolved bool) (*restoreFixture, reviewexec.ReviewRoute) {
	t.Helper()
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "route-cli", Provider: "coderabbit", Account: "route-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err = e.Configure(t.Context(), scope, "route-inventory-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "route-cli", Mode: "independent", Queue: []int64{4, 8}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(t.Context(), scope, "route-inventory-request", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal("route fixture enqueue", err)
	}
	var operation reviewexec.Receipt
	if err := json.Unmarshal(raw, &operation); err != nil {
		t.Fatal(err)
	}
	a, err := e.StartAttempt(t.Context(), f.project, operation.ID)
	if err != nil {
		t.Fatal("route fixture start", err)
	}
	at := time.Now().UTC()
	anchor := reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 4, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: at.Format(time.RFC3339Nano)}
	a, err = e.Admit(t.Context(), f.project, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal("route fixture admit", err)
	}
	captures, err := reviewruntime.NewCaptureStore(filepath.Join(f.dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := captures.Save(t.Context(), a.ID, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("Synthetic retained route finding"), Started: true}, StartedAt: at, FinishedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := e.RecordReviewEvidence(t.Context(), f.project, a.ID, reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 1, Findings: []reviewprovider.ParsedFinding{{Title: "Receiving policy finding", Detail: "Retained exact source", Severity: "low"}}, ReviewedFiles: []string{"example.go"}}})
	if err != nil {
		t.Fatal("route fixture evidence", err)
	}
	zero := int64(0)
	raw, err = e.CreateFindingRoute(t.Context(), scope, "route-inventory-create", evidence.ID, reviewexec.FindingRouteRequest{ExpectedRevision: &zero, Finding: 0, TargetPullNumber: 8, Reason: "Owned receiving work"})
	if err != nil {
		t.Fatal("route fixture create", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if resolved {
		if _, err := e.DecideFindingRoute(t.Context(), scope, "route-inventory-resolve", created.ID, reviewexec.FindingRouteDecisionRequest{ExpectedRevision: &one, Disposition: "rejected", Reason: "Resolved at receiver"}); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := e.ReadFindingRoute(t.Context(), scope, created.ID)
	if err != nil {
		t.Fatal("route fixture read", err)
	}
	var proofs int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_comparison_proofs`).Scan(&proofs); err != nil || proofs != 0 {
		t.Fatal("route fixture invented completed-operation comparison credit", proofs, err)
	}
	return f, detail.Route
}

func TestRoutePolicyInventoryRealRouteRoundTripIsReadOnly(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "resolved"}[resolved], func(t *testing.T) {
			f, route := routePolicyInventoryFixture(t, resolved)
			if err := reviewexec.ValidateRoutePolicyFacts(t.Context(), f.store.DB()); err != nil {
				t.Fatal("genuine route facts invalid", err)
			}
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-route-policy")
			if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := reviewexec.ValidateRoutePolicyFacts(t.Context(), db); err != nil {
				t.Fatal("inactive restore lost exact receiving policy", err)
			}
			var revision int64
			var active, pending bool
			if err := db.QueryRow(`SELECT revision,active,pending FROM review_route_policy_facts WHERE route_id=?`, route.ID).Scan(&revision, &active, &pending); err != nil || revision != route.Revision || !active || pending == resolved {
				t.Fatal("restore changed receiving classification", revision, active, pending, err)
			}
			retained := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && retained[table] != rows {
					t.Fatalf("restore changed route authority / history / facts: %s", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("backup operations mutated source or immutable bundle")
			}
		})
	}
}

func TestRoutePolicyInventoryRehashedFactAndInvalidationDamageRefused(t *testing.T) {
	var retain string
	for _, statement := range reviewexec.RoutePolicySchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_route_policy_facts_retain ") {
			retain = statement
		}
	}
	if retain == "" {
		t.Fatal("canonical retention trigger missing")
	}
	for _, tc := range []struct {
		name, sql string
		resolved  bool
		orphan    bool
	}{
		{"forged-resolved", `UPDATE review_route_policy_facts SET pending=0`, false, false},
		{"forged-pending", `UPDATE review_route_policy_facts SET pending=1`, true, false},
		{"forged-inactive", `UPDATE review_route_policy_facts SET active=0,pending=0`, true, false},
		{"stale-revision", `UPDATE review_route_policy_facts SET revision=revision+1`, true, false},
		{"wrong-target", `UPDATE review_route_policy_facts SET target_pull_number=target_pull_number+1`, true, false},
		{"wrong-project", `INSERT INTO projects VALUES('unrelated-route-project','Other',992,'synthetic/other'); UPDATE review_route_policy_facts SET project_id='unrelated-route-project'`, true, false},
		{"missing-fact", `DROP TRIGGER review_route_policy_facts_retain; DELETE FROM review_route_policy_facts; ` + retain, true, false},
		{"orphan-fact", `INSERT INTO review_route_policy_facts SELECT 'review_route_00000000000000000000000000000000',project_id,target_pull_number,revision,active,pending FROM review_route_policy_facts`, true, true},
		{"dirty-apparently-resolved", `UPDATE review_finding_routes SET body_json=replace(body_json,'Resolved at receiver','Raw changed receiver'); UPDATE review_finding_route_revisions SET body_json=replace(body_json,'Resolved at receiver','Raw changed receiver') WHERE revision=2`, true, false},
		{"stale-fact-with-cleared-invalidation", `UPDATE review_finding_routes SET disposition='accepted',body_json=replace(body_json,'"disposition":"rejected"','"disposition":"accepted"'); UPDATE review_finding_route_revisions SET body_json=replace(body_json,'"disposition":"rejected"','"disposition":"accepted"') WHERE revision=2; DELETE FROM review_route_policy_invalidations`, true, false},
		{"repeated-retag-invalidations", `UPDATE review_finding_routes SET target_pull_number=9; UPDATE review_finding_routes SET target_pull_number=10`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := routePolicyInventoryFixture(t, tc.resolved)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertCurrentSummaryInventoryRefused(t, bundle, !tc.orphan)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("damaged bundle changed source route authority")
			}
		})
	}
}

func TestRoutePolicyInventoryCanonicalSchemaAndTriggersRequired(t *testing.T) {
	tests := []struct{ name, sql string }{
		{"missing-index", `DROP INDEX review_route_policy_facts_pending`},
		{"missing-route-clear-index", `DROP INDEX review_route_policy_invalidations_route`},
		{"wrong-index", `DROP INDEX review_route_policy_facts_pending; CREATE INDEX review_route_policy_facts_pending ON review_route_policy_facts(route_id)`},
		{"extra-index", `CREATE INDEX extra_route_policy_index ON review_route_policy_facts(revision)`},
		{"missing-authority-trigger", `DROP TRIGGER review_route_policy_dirty_review_finding_route_revisions_update`},
		{"missing-retain-trigger", `DROP TRIGGER review_route_policy_facts_retain`},
		{"changed-trigger", `DROP TRIGGER review_route_policy_dirty_review_finding_routes_update; CREATE TRIGGER review_route_policy_dirty_review_finding_routes_update AFTER UPDATE ON review_finding_routes BEGIN SELECT 1; END`},
		{"extra-trigger", `CREATE TRIGGER unexpected_route_policy BEFORE INSERT ON review_route_policy_facts BEGIN SELECT 1; END`},
		{"extra-column", `ALTER TABLE review_route_policy_facts ADD COLUMN invented_authority INTEGER`},
		{"wrong-domain-version", `UPDATE metadata SET value='2' WHERE key='schema:review_route_policy'`},
	}
	var table, retain, index string
	for _, statement := range reviewexec.RoutePolicySchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TABLE review_route_policy_facts ") {
			table = statement
		} else if strings.HasPrefix(statement, "CREATE TRIGGER review_route_policy_facts_retain ") {
			retain = statement
		} else if strings.HasPrefix(statement, "CREATE INDEX review_route_policy_facts_pending ") {
			index = statement
		}
	}
	for _, change := range []struct{ name, old, replacement string }{
		{"nullable-route-key", "route_id TEXT NOT NULL PRIMARY KEY", "route_id TEXT PRIMARY KEY"},
		{"wrong-primary-key", "route_id TEXT NOT NULL PRIMARY KEY", "route_id TEXT NOT NULL UNIQUE"},
		{"changed-foreign-key", "REFERENCES review_finding_routes(id)", "REFERENCES review_finding_routes(id) ON DELETE CASCADE"},
		{"changed-pending-check", "pending IN (0,1) AND pending<=active", "pending IN (0,1)"},
	} {
		changed := strings.Replace(table, change.old, change.replacement, 1)
		if changed == table || table == "" || retain == "" || index == "" {
			t.Fatal("missing canonical schema fixture", change.name)
		}
		tests = append(tests, struct{ name, sql string }{change.name, `DROP TABLE review_route_policy_facts; ` + changed + index + retain})
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

func TestRoutePolicyInventoryAbsentOptionalDomainRemainsSupported(t *testing.T) {
	store, dir := historyInventoryFixture(t)
	before := backupRows(t, store.DB())
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "predecessor-bundles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("supported predecessor without route policy refused", err)
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
	if err := db.QueryRow(`SELECT count(*) FROM metadata WHERE key='schema:review_route_policy'`).Scan(&added); err != nil || added != 0 {
		t.Fatal("predecessor inspection / restore installed optional schema", added, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
		t.Fatal("predecessor source changed")
	}
}
