package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
)

// Every current fixture uses the actual owning startup migration. Canonical-layout
// and roundtrip proof must not silently fall back to hand-installed future DDL.
func historyInventoryFixture(t *testing.T) (*core.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = reviews.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err = store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "11" {
		t.Fatal("owning startup must install reviews v11", version, err)
	}
	return store, dir
}

func TestHistoryInventoryV6CanonicalEmptySchemaIsReadOnly(t *testing.T) {
	store, dir := historyInventoryFixture(t)
	before := backupRows(t, store.DB())
	versions, err := validateSchema(t.Context(), store.DB(), false)
	if err != nil || versions["reviews"] != 11 {
		t.Fatal("canonical v6 refused", versions, err)
	}
	// The five/six-column groups are checked independently of exact DDL too.
	for _, table := range []string{"review_current_runs", "review_current_attempts", "review_attempt_memberships", "review_current_run_order"} {
		if err = validateForeignKeys(t.Context(), store.DB(), table); err != nil {
			t.Fatalf("canonical grouped FK refused %s: %v", table, err)
		}
	}
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection or schema validation changed canonical state")
	}
}

func historyTableDefinition(t *testing.T, name string) string {
	t.Helper()
	definitions := append(reviews.HistorySchemaSQL(), reviews.CurrentRunOrderSchemaSQL()...)
	for _, statement := range definitions {
		if strings.HasPrefix(statement, "CREATE TABLE "+name+" (") {
			return statement
		}
	}
	t.Fatal("missing canonical test table", name)
	return ""
}

func TestHistoryInventoryV6RehashedSchemaViewsAndIndexesRefused(t *testing.T) {
	tests := []struct{ name, sql string }{
		{"unknown-view", `CREATE VIEW private_dump AS SELECT * FROM credentials`},
		{"missing-view", `DROP VIEW review_findings`},
		{"changed-view", `DROP VIEW review_findings; CREATE VIEW review_findings AS SELECT 1 AS detail`},
		{"oversized-view", `DROP VIEW review_findings; CREATE VIEW review_findings AS SELECT 1 AS detail /*` + strings.Repeat("x", 16384) + `*/`},
		{"writable-view", `CREATE TRIGGER writable_history INSTEAD OF INSERT ON review_pulls BEGIN SELECT 1; END`},
		{"unknown-trigger", `CREATE TRIGGER unknown_history BEFORE INSERT ON review_current_pulls BEGIN SELECT 1; END`},
		{"missing-open-index", `DROP INDEX review_run_memberships_open`},
		{"changed-open-index", `DROP INDEX review_run_memberships_open; CREATE UNIQUE INDEX review_run_memberships_open ON review_run_memberships(project_id,id)`},
		{"unknown-index", `CREATE INDEX unknown_history_index ON review_current_runs(project_id,id)`},
		{"extra-column", `ALTER TABLE review_run_versions ADD COLUMN ignored TEXT`},
		{"missing-table", `DROP TABLE review_current_attempts`},
		{"missing-run-order-table", `DROP TABLE review_current_run_order`},
		{"missing-run-order-index", `DROP INDEX review_current_run_order_eligible`},
		{"wrong-run-order-index", `DROP INDEX review_current_run_order_eligible; CREATE INDEX review_current_run_order_eligible ON review_current_run_order(project_id,pull_number,channel,run_order ASC) WHERE outcome='completed' AND finalized=1`},
		{"wrong-run-order-predicate", `DROP INDEX review_current_run_order_eligible; CREATE INDEX review_current_run_order_eligible ON review_current_run_order(project_id,pull_number,channel,run_order DESC) WHERE outcome='completed'`},
		{"wrong-version", `UPDATE metadata SET value='3' WHERE key='schema:reviews'`},
		{"future-version", `UPDATE metadata SET value='12' WHERE key='schema:reviews'`},
	}
	for _, test := range []struct{ name, table, old, new string }{
		{"missing-order-check", "review_current_pulls", "CHECK (position >= 0)", "CHECK (position >= -1)"},
		{"changed-order-unique", "review_current_pulls", "UNIQUE (project_id,position)", "UNIQUE (project_id,number,position)"},
		{"changed-primary-key", "review_current_retired", "PRIMARY KEY (project_id,number)", "PRIMARY KEY (project_id,number,start_revision)"},
		{"changed-six-column-fk", "review_current_attempts", "ON UPDATE NO ACTION", "ON UPDATE CASCADE"},
		{"changed-default", "review_finding_versions", "DEFAULT 'null'", "DEFAULT '{}'"},
		{"changed-run-order-check", "review_current_run_order", "CHECK(finalized IN(0,1))", "CHECK(finalized IN(0,1,2))"},
		{"changed-run-order-cascade", "review_current_run_order", "ON DELETE CASCADE", "ON DELETE NO ACTION"},
	} {
		definition := historyTableDefinition(t, test.table)
		changed := strings.Replace(definition, test.old, test.new, 1)
		if changed == definition {
			t.Fatal("unchanged malformed fixture", test.name)
		}
		tests = append(tests, struct{ name, sql string }{test.name, `PRAGMA foreign_keys=OFF; DROP TABLE ` + test.table + `; ` + changed})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, bundle.Path, test.sql)
			immutable := restoreTree(t, bundle.Path)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			destination := filepath.Join(t.TempDir(), "must-not-create")
			_, err = Restore(t.Context(), bundle.Path, destination)
			backupCode(t, err, "backup_invalid")
			if _, err = os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid v6 schema reached restore writes")
			}
			if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("rejection modified retained bundle")
			}
		})
	}
}

func TestHistoryInventoryLegacyVersionsRejectViews(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "source")
			store, err := core.Open(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = installLegacyReviewInventorySchema(t.Context(), store, version); err != nil {
				t.Fatal(err)
			}
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, bundle.Path, `CREATE VIEW extra_legacy_view AS SELECT 1`)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			if version >= 2 {
				bundle, err = Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
				if err != nil {
					t.Fatal(err)
				}
				mutateBundleSQL(t, bundle.Path, `DROP TRIGGER review_runs_snapshot_revision_insert; CREATE TRIGGER review_runs_snapshot_revision_insert BEFORE INSERT ON review_runs BEGIN SELECT 1; END`)
				_, err = Inspect(t.Context(), bundle.Path)
				backupCode(t, err, "backup_invalid")
			}
		})
	}
}

func historyInventorySeedPull(t *testing.T, store *core.Store) {
	t.Helper()
	if _, err := store.DB().Exec(`INSERT INTO projects VALUES('history-private','Private sentinel',701,'owner/history');
 INSERT INTO project_handles VALUES('history-private','private-sentinel');
 INSERT INTO review_snapshots VALUES('history-private','review_snapshot_11111111111111111111111111111111',1,701,'synthetic-import','','','');
 INSERT INTO review_workspace VALUES('history-private',1,'review_snapshot_11111111111111111111111111111111');
 INSERT INTO review_pull_versions VALUES('history-private',7,1,'Retained merged PR','merged','','main','','success','mergeable',1,0,0,'[]','[]','{}',1);
 INSERT INTO review_pull_memberships VALUES('history-private',7,1,NULL,1,0);
 INSERT INTO review_current_pulls VALUES('history-private',7,1,0);
 INSERT INTO review_current_totals(project_id,pull_number,run_count,attempt_count,incomplete_run_count) VALUES('history-private',7,0,0,0)`); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryInventoryV6RehashedReferenceGraphRefused(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"missing-current", `DELETE FROM review_current_totals; DELETE FROM review_current_pulls`},
		{"missing-workspace", `DELETE FROM review_workspace`},
		{"wrong-workspace-revision", `UPDATE review_workspace SET revision=2`},
		{"uncommitted-future-snapshot", `INSERT INTO review_snapshots VALUES('history-private','review_snapshot_22222222222222222222222222222222',2,701,'synthetic-import','','','')`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			historyInventorySeedPull(t, store)
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, bundle.Path, test.sql)
			before := restoreTree(t, bundle.Path)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("reference validation wrote bundle")
			}
		})
	}
}

func TestHistoryInventoryV6InstalledSchemaCreateInspectRestore(t *testing.T) {
	store, dir := historyInventoryFixture(t)
	// Source initialization uses the actual owner migration, then real bundle IO.
	historyInventorySeedPull(t, store)
	original := backupRows(t, store.DB())
	var signing string
	var sourceCredentials int
	if err := store.DB().QueryRow(`SELECT value,(SELECT count(*) FROM credentials) FROM metadata WHERE key='signing_key'`).Scan(&signing, &sourceCredentials); err != nil || sourceCredentials < 1 {
		t.Fatal("source identity/access fixture unavailable", err)
	}
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" || restored.SourceInstanceID != store.InstanceID() || restored.InstanceID == store.InstanceID() {
		t.Fatal(restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retained := backupRows(t, db)
	var instance, nextSigning, state string
	var activeCredentials, retainedCredentials int
	if err := db.QueryRow(`SELECT (SELECT value FROM metadata WHERE key='instance_id'),(SELECT value FROM metadata WHERE key='signing_key'),(SELECT value FROM metadata WHERE key='restore:state'),(SELECT count(*) FROM credentials WHERE revoked!=1),(SELECT count(*) FROM credentials)`).Scan(&instance, &nextSigning, &state, &activeCredentials, &retainedCredentials); err != nil || instance != restored.InstanceID || nextSigning == signing || nextSigning == "" || state != "inactive" || activeCredentials != 0 || retainedCredentials != sourceCredentials {
		t.Fatal("inactive restore identity/access/provider hold changed", err)
	}
	for _, table := range []string{"projects", "review_snapshots", "review_workspace", "review_pull_versions", "review_pull_memberships", "review_current_pulls"} {
		got := retained[table]
		if got != original[table] {
			t.Fatalf("restoration changed %s", table)
		}
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("roundtrip changed source bundle")
	}
}

// This exercises the actual owner APIs and transactional projection helpers.
// A synthetic PublishNative call proves projection preservation, not captured
// execution credit; the raw journal/capture roundtrip has separate fixtures.
func historyInventoryAPIFixture(t *testing.T) *restoreFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = reviews.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	tokenBody, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &restoreFixture{dir: dir, store: store, token: strings.TrimSpace(string(tokenBody))}
	f.handler = server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 991, FullName: "synthetic/history"}, nil
	}))
	reviews.Register(f.handler.(*http.ServeMux), store)
	f.project = f.request(t, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Private historical fixture", "repository": "synthetic/history"})["id"].(string)
	if err = store.DB().QueryRow(`SELECT principal_id FROM credentials WHERE is_admin=1`).Scan(&f.principal); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHistoryInventoryV6MixedHistoryReadsAndAdjudicationSurviveRestore(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	prefix := "/api/v1/projects/" + f.project + "/reviews"
	read := func(handler http.Handler, store *core.Store, token, route string, status int) []byte {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, prefix+route, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Instance", store.InstanceID())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("read %s: status %d, want%d: %s", route, w.Code, status, w.Body.String())
		}
		return bytes.Clone(w.Body.Bytes())
	}
	decode := func(raw []byte, to any) {
		t.Helper()
		if err := json.Unmarshal(raw, to); err != nil {
			t.Fatal(err)
		}
	}
	duration := int64(60)
	finding := reviews.Finding{SourceFindingID: "shared-finding", Title: "Retained literal <finding>", Detail: "IMPORTED_PRIVATE_TEXT", Severity: "high", Disposition: "accepted", DecisionReason: "Retained import decision", Actor: "Import author", DecidedAt: "2026-01-01T00:02:00Z", FixSHA: strings.Repeat("d", 40), ProofNote: "Imported exact fix", ResolvedAt: "2026-01-01T00:03:00Z"}
	imported := reviews.RunInput{SourceRunID: "shared-source-run", Channel: "manual", HeadSHA: strings.Repeat("a", 40), Reviewer: "Historical reviewer", Scope: "Exact imported comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Accepted: 1, Finalized: true, CoverageLimits: []string{"Historical coverage remains imported"}, Findings: []reviews.Finding{finding}}
	pull := reviews.PullInput{Number: 11, Title: "Merged history remains visible", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: true, Runs: []reviews.RunInput{imported}, Attempts: []reviews.Attempt{{SourceAttemptID: "shared-attempt", Channel: "manual", HeadSHA: strings.Repeat("a", 40), State: "completed", StartedAt: imported.StartedAt, FinishedAt: imported.FinishedAt, DurationSeconds: &duration, SourceRunID: imported.SourceRunID}}}
	other := pull
	other.Number = 12
	other.Title = "Other retained PR"
	other.Runs = nil
	other.Attempts = nil
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT COALESCE((SELECT revision FROM review_workspace WHERE project_id=?),0)`, f.project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	observed := f.request(t, http.MethodPost, prefix+"/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "historical-producer", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull, other}, Retired: []reviews.RetiredPull{}})
	importedSnapshot := observed["id"].(string)
	var queue reviews.QueueResponse
	decode(read(f.handler, f.store, f.token, "/queue?limit=1", 200), &queue)
	if queue.SnapshotID != importedSnapshot || !queue.HasMore || queue.Cursor == "" {
		t.Fatal("missing genuine pinned queue cursor")
	}
	queueContinuation := "/queue?limit=1&cursor=" + url.QueryEscape(queue.Cursor)
	pinnedQueueNext := read(f.handler, f.store, f.token, queueContinuation, 200)
	var importedRuns reviews.RunsResponse
	decode(read(f.handler, f.store, f.token, "/pulls/11/runs?snapshot="+importedSnapshot, 200), &importedRuns)
	importedDetailRoute := "/runs/" + importedRuns.Items[0].ID + "?snapshot=" + importedSnapshot
	importedDetail := read(f.handler, f.store, f.token, importedDetailRoute, 200)
	// Matching external IDs in distinct producers must remain distinct identities.
	native := imported
	native.Channel = "cli"
	native.Reviewer = "Synthetic native projection"
	native.Finalized = false
	native.Accepted = 0
	native.CoverageLimits = []string{"Projection fixture; no capture authority claim"}
	native.Findings = []reviews.Finding{{SourceFindingID: "shared-finding", Title: "Native literal finding", Detail: "NATIVE_IMMUTABLE_TEXT", Severity: "high", Disposition: "unadjudicated"}}
	metadata := pull
	metadata.Runs = nil
	metadata.Attempts = nil
	attempt := reviews.Attempt{SourceAttemptID: "shared-attempt", Channel: "cli", HeadSHA: native.HeadSHA, State: "completed", StartedAt: native.StartedAt, FinishedAt: native.FinishedAt, DurationSeconds: &duration, SourceRunID: native.SourceRunID}
	scope := core.Scope{ProjectID: f.project, PrincipalID: f.principal, WorkerID: f.worker}
	var beforeDecision reviews.Receipt
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		beforeDecision, err = reviews.PublishNative(ctx, tx, scope, reviews.NativePublication{Pull: metadata, Run: &native, Attempt: &attempt})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var nativeRuns reviews.RunsResponse
	decode(read(f.handler, f.store, f.token, "/pulls/11/runs", 200), &nativeRuns)
	if len(nativeRuns.Items) != 2 || nativeRuns.Items[0].Source != "native-executor" || nativeRuns.Items[1].Source != "historical-producer" || nativeRuns.Items[0].ID == nativeRuns.Items[1].ID {
		t.Fatal("mixed producer identity lost", nativeRuns)
	}
	nativeID := nativeRuns.Items[0].ID
	oldNativeRoute := "/runs/" + nativeID + "?snapshot=" + beforeDecision.ID
	oldNative := read(f.handler, f.store, f.token, oldNativeRoute, 200)
	var afterDecision reviews.Receipt
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var changed bool
		var err error
		afterDecision, changed, err = reviews.UpdateNativeAdjudication(ctx, tx, scope, reviews.NativeAdjudication{SourceRunID: native.SourceRunID, Finalized: true, FrozenCounts: &reviews.NativeCounts{Found: 1, Accepted: 1}, Findings: []reviews.NativeFindingUpdate{{SourceFindingID: "shared-finding", Disposition: "accepted", DecisionReason: "Native final decision", Actor: "Native adjudicator", DecidedAt: "2026-01-01T00:05:00Z", FixSHA: strings.Repeat("e", 40), ProofNote: "Native proof remains exact", ResolvedAt: "2026-01-01T00:06:00Z"}}})
		if err == nil && !changed {
			t.Fatal("native adjudication did not append a version")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if afterDecision.Revision != beforeDecision.Revision+1 {
		t.Fatal("adjudication did not advance once")
	}
	if !bytes.Equal(pinnedQueueNext, read(f.handler, f.store, f.token, queueContinuation, 200)) || !bytes.Equal(importedDetail, read(f.handler, f.store, f.token, importedDetailRoute, 200)) || !bytes.Equal(oldNative, read(f.handler, f.store, f.token, oldNativeRoute, 200)) {
		t.Fatal("later publication changed pinned historical response")
	}
	routes := []string{"/queue", "/pulls/11", "/pulls/11?snapshot=" + importedSnapshot, "/pulls/11/runs?snapshot=" + beforeDecision.ID, "/pulls/11/attempts?snapshot=" + beforeDecision.ID, importedDetailRoute, oldNativeRoute, "/runs/" + nativeID, "/pulls/11/attempts?limit=1"}
	responses := make(map[string][]byte, len(routes))
	for _, route := range routes {
		responses[route] = read(f.handler, f.store, f.token, route, 200)
	}
	var latest reviews.RunDetailResponse
	decode(responses["/runs/"+nativeID], &latest)
	if !latest.Run.Finalized || latest.Run.Fixed != 1 || latest.Findings[0].Detail != "NATIVE_IMMUTABLE_TEXT" || latest.Findings[0].ProofNote != "Native proof remains exact" {
		t.Fatal("adjudicated proof missing")
	}
	var indexedRuns, eligibleRuns int
	if err = f.store.DB().QueryRow(`SELECT count(*),sum(CASE WHEN channel='cli' AND outcome='completed' AND finalized=1 THEN 1 ELSE 0 END) FROM review_current_run_order WHERE project_id=?`, f.project).Scan(&indexedRuns, &eligibleRuns); err != nil || indexedRuns != 2 || eligibleRuns != 1 {
		t.Fatal("mixed source/current adjudication cache was not retained", indexedRuns, eligibleRuns, err)
	}
	original := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, backupRows(t, f.store.DB())) {
		t.Fatal("backup/inspection changed live source")
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" {
		t.Fatal(restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	retained := backupRows(t, db)
	db.Close()
	for table, before := range original {
		if table != "metadata" && table != "credentials" && retained[table] != before {
			t.Fatalf("inactive restore changed retained table %s", table)
		}
	}
	// Fresh explicit coordination activation rotates access; it must not reactivate
	// predecessor cursors or create execution authority from projection labels.
	token := "synthetic-history-activation-token-1234567890"
	tokenFile := filepath.Join(t.TempDir(), "fresh-admin.token")
	if err = os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Activate(t.Context(), destination, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: tokenFile}); err != nil {
		t.Fatal(err)
	}
	reopened := activationOpen(t, destination)
	mux := http.NewServeMux()
	reviews.Register(mux, reopened)
	read(mux, reopened, token, queueContinuation, 400)
	normalize := func(raw []byte) string {
		t.Helper()
		var value map[string]any
		decode(raw, &value)
		delete(value, "cursor")
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for _, route := range routes {
		if normalize(responses[route]) != normalize(read(mux, reopened, token, route, 200)) {
			t.Fatalf("restored response changed %s", route)
		}
	}
	var page reviews.AttemptsResponse
	decode(read(mux, reopened, token, "/pulls/11/attempts?limit=1&snapshot="+beforeDecision.ID, 200), &page)
	if !page.HasMore || page.Cursor == "" || page.Items[0].Source != "native-executor" {
		t.Fatal("restored archive first page lost source or continuation")
	}
	var page2 reviews.AttemptsResponse
	decode(read(mux, reopened, token, "/pulls/11/attempts?limit=1&cursor="+url.QueryEscape(page.Cursor), 200), &page2)
	if page2.SnapshotID != beforeDecision.ID || len(page2.Items) != 1 || page2.Items[0].Source != "historical-producer" || page2.HasMore {
		t.Fatal("restored archive continuation lost original producer")
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("restore/activation modified original bundle")
	}
}

func TestHistoryInventoryV6RehashedOwningPullVisibilityDamageRefused(t *testing.T) {
	for _, kind := range []string{"run", "attempt"} {
		for _, damage := range []string{"missing-parent", "historical-parent-gap"} {
			t.Run(kind+"/"+damage, func(t *testing.T) {
				f := historyInventoryAPIFixture(t)
				pull := reviews.PullInput{Number: 7, Title: "Owning PR", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: true}
				if kind == "run" {
					pull.Runs = []reviews.RunInput{{SourceRunID: "owning-parent-run", Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Synthetic retained reviewer", Scope: "Exact source", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", CoverageLimits: []string{}, Findings: []reviews.Finding{}}}
				} else {
					pull.Attempts = []reviews.Attempt{{SourceAttemptID: "owning-parent-attempt", Channel: "cli", HeadSHA: pull.HeadSHA, State: "prepared", StartedAt: "2026-01-01T00:00:00Z"}}
				}
				// Change only the PR body three times: the retained child remains visible
				// across all three contiguous parent versions. This valid source must back up.
				for revision := int64(0); revision < 3; revision++ {
					pull.Title = fmt.Sprintf("Owning PR body%d", revision)
					f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "parent-visibility-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull}, Retired: []reviews.RetiredPull{}})
				}
				beforeSource := backupRows(t, f.store.DB())
				bundle := restoreBundle(t, f)
				if _, err := Inspect(t.Context(), bundle.Path); err != nil {
					t.Fatal("contiguous changed parent rejected", err)
				}
				if damage == "missing-parent" {
					mutateBundleSQL(t, bundle.Path, `DELETE FROM review_current_totals; DELETE FROM review_current_pulls; DELETE FROM review_pull_memberships`)
				} else {
					mutateBundleSQL(t, bundle.Path, `DELETE FROM review_pull_memberships WHERE start_revision=2`)
				}
				// Resealing repairs all file/manifest hashes. Exact physical layout and all
				// SQLite FKs must remain valid, isolating the cross-family visibility fault.
				db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = validateSchema(t.Context(), db, false); err != nil {
					db.Close()
					t.Fatal("damage must preserve canonical physical schema", err)
				}
				rows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				if rows.Next() {
					rows.Close()
					db.Close()
					t.Fatal("damage must preserve ordinary foreign keys")
				}
				if err = rows.Err(); err != nil {
					rows.Close()
					db.Close()
					t.Fatal(err)
				}
				rows.Close()
				db.Close()
				immutable := restoreTree(t, bundle.Path)
				_, err = Inspect(t.Context(), bundle.Path)
				backupCode(t, err, "backup_invalid")
				destination := filepath.Join(t.TempDir(), "must-not-create")
				_, err = Restore(t.Context(), bundle.Path, destination)
				backupCode(t, err, "backup_invalid")
				if _, err = os.Lstat(destination); !os.IsNotExist(err) {
					t.Fatal("invalid ownership graph reached restore writes")
				}
				if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
					t.Fatal("rejected inspection/restore modified damaged bundle")
				}
				if !reflect.DeepEqual(beforeSource, backupRows(t, f.store.DB())) {
					t.Fatal("negative proof modified valid source")
				}
			})
		}
	}
}

// Reconstruct only the truthful older derived schema in an exclusively owned
// fixture; no retained source/history bytes or IDs are rewritten or migrated by
// backup creation, inspection or inactive restore.
func TestHistoryInventoryV4V5V6RemainSupportedWithoutFutureMetadata(t *testing.T) {
	for _, version := range []int{4, 5, 6} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := historyInventoryRunOrderFixture(t)
			removeChannelRunCountsForLegacyFixture(t, f.store)
			if version < 6 {
				if _, err := f.store.DB().Exec(`DROP TABLE review_current_run_order`); err != nil {
					t.Fatal(err)
				}
			}
			if version == 4 {
				if _, err := f.store.DB().Exec(`DROP TABLE review_current_attempt_order; DROP TABLE review_current_totals`); err != nil {
					t.Fatal(err)
				}
			}
			for _, statement := range []string{`DROP VIEW review_attempts`, `ALTER TABLE review_attempt_versions DROP COLUMN reason`, reviews.HistoryReadViewSQLV7()["review_attempts"], `DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN metadata_json`, reviews.HistoryReadViewSQLV6()["review_pulls"]} {
				if _, err := f.store.DB().Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.store.DB().Exec(`UPDATE metadata SET value=? WHERE key='schema:reviews'`, fmt.Sprint(version)); err != nil {
				t.Fatal(err)
			}
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("supported older bundle refused", err)
			}
			target := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, target)
			if err != nil || restored.State != "inactive" {
				t.Fatal(restored, err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			after := backupRows(t, db)
			var installed int
			err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='review_current_run_order'`).Scan(&installed)
			db.Close()
			if err != nil || (installed == 1) != (version == 6) {
				t.Fatal("inspection/restore installed a future cache", err)
			}
			for table, hash := range before {
				if table != "metadata" && table != "credentials" && after[table] != hash {
					t.Fatal("old retained rows changed", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("older source/bundle mutated")
			}
		})
	}
}

func historyInventoryRunOrderFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := historyInventoryAPIFixture(t)
	var revision int64
	f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{
		ExpectedRevision: &revision, RepositoryID: 991, Source: "original-cache-source", ObservedAt: "2026-01-01T00:00:00Z",
		Queue: []reviews.PullInput{{Number: 7, Title: "Retained current run metadata", State: "merged", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{
			{SourceRunID: "eligible-source-run", Channel: "cli", Scope: "retained selected evidence", Outcome: "completed", Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z"},
			{SourceRunID: "ineligible-source-run", Channel: "hosted", Scope: "retained unknown outcome", Outcome: "unknown"},
		}}},
	})
	return f
}

func TestHistoryInventoryV6RehashedCurrentRunOrderDamageRefused(t *testing.T) {
	for _, damage := range []struct{ name, sql string }{
		{"missing", `DELETE FROM review_current_run_order WHERE channel='cli'`},
		{"channel", `UPDATE review_current_run_order SET channel='manual' WHERE channel='cli'`},
		{"outcome", `UPDATE review_current_run_order SET outcome='failed' WHERE channel='cli'`},
		{"finalized", `UPDATE review_current_run_order SET finalized=0 WHERE channel='cli'`},
		{"order", `UPDATE review_current_run_order SET run_order=run_order+10 WHERE channel='cli'`},
		{"owner", `UPDATE review_current_run_order SET pull_number=8 WHERE channel='cli'`},
	} {
		t.Run(damage.name, func(t *testing.T) {
			f := historyInventoryRunOrderFixture(t)
			source := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, damage.sql)
			// Exact physical layout and the full retained reference graph remain valid;
			// the new derived cache is the only damaged data, not source authority.
			db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			versions, err := validateSchema(t.Context(), db, false)
			if err != nil || versions["reviews"] != 11 {
				t.Fatal("logical fixture changed canonical physical layout", versions, err)
			}
			fk, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
			if err != nil {
				t.Fatal(err)
			}
			hasFK := fk.Next()
			fkErr := fk.Err()
			fk.Close()
			if hasFK || fkErr != nil {
				t.Fatal("logical fixture broke physical FK", fkErr)
			}
			if err = reviews.ValidateHistoryReferences(t.Context(), db); err != nil {
				t.Fatal("logical fixture changed retained source", err)
			}
			if err = reviews.ValidateCurrentSummaries(t.Context(), db); err != nil {
				t.Fatal("logical fixture changed old caches", err)
			}
			if err = reviews.ValidateCurrentRunOrder(t.Context(), db); err == nil {
				t.Fatal("derived current cache damage accepted")
			}
			db.Close()
			before := restoreTree(t, bundle.Path)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			target := filepath.Join(t.TempDir(), "must-not-create")
			_, err = Restore(t.Context(), bundle.Path, target)
			backupCode(t, err, "backup_invalid")
			if _, err = os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("corrupt derived cache reached restore writes")
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(source, backupRows(t, f.store.DB())) {
				t.Fatal("corruption refusal mutated source or sealed bundle")
			}
		})
	}
}

func TestHistoryInventoryV6ExtraUnreferencedCacheRowRefused(t *testing.T) {
	f := historyInventoryRunOrderFixture(t)
	bundle := restoreBundle(t, f)
	// A literal surplus row cannot retain its current-run FK when every current
	// run already owns exactly one cache row. This case deliberately tests that
	// physical refusal separately from FK-valid field/missing-row damage above.
	mutateBundleSQL(t, bundle.Path, `INSERT INTO review_current_run_order SELECT project_id,'review_run_ffffffffffffffffffffffffffffffff',pull_number,channel,outcome,finalized,run_order FROM review_current_run_order LIMIT 1`)
	before := restoreTree(t, bundle.Path)
	_, err := Inspect(t.Context(), bundle.Path)
	backupCode(t, err, "backup_invalid")
	target := filepath.Join(t.TempDir(), "must-not-create")
	_, err = Restore(t.Context(), bundle.Path, target)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(target); !os.IsNotExist(err) || !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("extra row refusal changed destination/bundle")
	}
}

func TestHistoryInventoryV6LiveCacheCorruptionRefusesCreateAndRetries(t *testing.T) {
	f := historyInventoryRunOrderFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE review_current_run_order SET finalized=0 WHERE channel='cli'`); err != nil {
		t.Fatal(err)
	}
	corrupt := backupRows(t, f.store.DB())
	root := filepath.Join(t.TempDir(), "bundles")
	_, err := Create(t.Context(), f.store, f.dir, root)
	backupCode(t, err, "backup_invalid")
	if !reflect.DeepEqual(corrupt, backupRows(t, f.store.DB())) {
		t.Fatal("failed creation repaired current cache")
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("failed validation published a bundle")
	}
	// Restore the exact fixture value; ordinary creation then retries normally.
	if _, err = f.store.DB().Exec(`UPDATE review_current_run_order SET finalized=1 WHERE channel='cli'`); err != nil {
		t.Fatal(err)
	}
	bundle, err := Create(t.Context(), f.store, f.dir, root)
	if err != nil {
		t.Fatal("corrected fixture could not retry", err)
	}
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
}
