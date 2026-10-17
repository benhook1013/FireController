package reviews

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
)

func importBase(f *apiFixture) string { return "/api/v1/projects/" + f.project + "/reviews" }

func importHTTP(f *apiFixture, path, token, worker, key string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func importJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func requireImportResponse(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || code != "" && !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("response want %d/%s, got %d %s", status, code, response.Code, response.Body.String())
	}
}

func previewFor(t *testing.T, f *apiFixture, req ObservationRequest) ImportPreview {
	t.Helper()
	w := importHTTP(f, importBase(f)+"/import/preview", f.admin, "", "", importJSON(t, req))
	requireImportResponse(t, w, 200, "")
	var preview ImportPreview
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func cloneImport(t *testing.T, req ObservationRequest) ObservationRequest {
	t.Helper()
	var out ObservationRequest
	if err := json.Unmarshal(importJSON(t, req), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Compare every table's values, not just counts: preview may not replace a
// pointer, change a receipt, mark a message or edit any retained history.
func importDatabaseState(t *testing.T, f *apiFixture) map[string][]string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]string)
	for _, table := range tables {
		rows, err := f.store.DB().Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out[table] = []string{}
		for rows.Next() {
			values, dest := make([]any, len(columns)), make([]any, len(columns))
			for i := range dest {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], string(importJSON(t, values)))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}

func fullImportRequest() ObservationRequest {
	pull := samplePull(7, "PRIVATE_IMPORT_TITLE <script>")
	pull.Runs = []RunInput{{SourceRunID: "source-run-7", Channel: "hosted", Scope: "full", Outcome: "completed",
		Complete: true, Finalized: true, Attributable: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Accepted: 1,
		Findings: []Finding{{SourceFindingID: "source-finding", Title: "PRIVATE_FINDING_TITLE", Detail: "PRIVATE_FINDING_BODY", Severity: "medium", Disposition: "accepted"}}}}
	pull.Attempts = []Attempt{{SourceAttemptID: "source-attempt", Channel: "hosted", State: "completed", SourceRunID: "source-run-7", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z"}}
	req := reviewRequest(0, []PullInput{pull, samplePull(8, "Another merged PR")})
	req.ObservedAt = "2026-01-02T00:00:00Z"
	return req
}

func TestReviewImportPreviewConservesAllStateAndNormalizes(t *testing.T) {
	f := newAPIFixture(t)
	if err := jobs.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO workers VALUES('project-review-test','worker-import','Importer')`,
		`INSERT INTO jobs VALUES('project-review-test','job-retained','Original','Private brief','worker-import','active',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	} {
		if _, err := f.store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	req := fullImportRequest()
	before := importDatabaseState(t, f)
	response := importHTTP(f, importBase(f)+"/import/preview", f.admin, "", "ignored-preview-key", importJSON(t, req))
	requireImportResponse(t, response, 200, "")
	var preview ImportPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Version != 1 || preview.ProjectID != f.project || preview.RepositoryID != 701 || preview.Source != req.Source || preview.ObservedAt != req.ObservedAt || preview.ExpectedRevision != 0 || !preview.ImportedHistory || preview.NativeCoverage || preview.Counts != (ImportCounts{Pulls: 2, Runs: 1, Attempts: 1, Findings: 1}) {
		t.Fatalf("incorrect preview: %+v", preview)
	}
	if len(response.Body.Bytes()) > 4096 || strings.Contains(response.Body.String(), "PRIVATE_") || strings.Contains(response.Body.String(), f.admin) {
		t.Fatal("preview exposed source text/credentials or was not compact")
	}
	normalized := cloneImport(t, req)
	body, err := normalizeObservation(&normalized)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	if preview.PayloadSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("payload digest differs from stored canonical observation encoding")
	}
	pretty, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	prettyResponse := importHTTP(f, importBase(f)+"/import/preview", f.admin, "", "", pretty)
	requireImportResponse(t, prettyResponse, 200, "")
	if prettyResponse.Body.String() != response.Body.String() {
		t.Fatal("whitespace or null-to-empty collection spelling changed preview")
	}
	if after := importDatabaseState(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("preview changed database state")
	}
}

func TestReviewImportFingerprintBindsContentAndDestination(t *testing.T) {
	f := newAPIFixture(t)
	req := fullImportRequest()
	original := previewFor(t, f, req)
	// Bind principal and optional worker even though both profiles have admin
	// authority over the same project and may read the same imported source.
	otherToken := "second-import-administrator"
	sum := sha256.Sum256([]byte(otherToken))
	if _, err := f.store.DB().Exec(`INSERT INTO credentials VALUES('second-admin','second-principal',?,1,0)`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO workers VALUES(?,'worker-import','Import worker')`, f.project); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, token, worker string }{{"principal", otherToken, ""}, {"worker", f.admin, "worker-import"}} {
		t.Run(test.name, func(t *testing.T) {
			response := importHTTP(f, importBase(f)+"/import/preview", test.token, test.worker, "", importJSON(t, req))
			requireImportResponse(t, response, 200, "")
			var other ImportPreview
			if json.Unmarshal(response.Body.Bytes(), &other) != nil || other.PayloadSHA256 != original.PayloadSHA256 || other.PreviewSHA256 == original.PreviewSHA256 {
				t.Fatal("scope did not change preview binding independently of payload")
			}
			before := importDatabaseState(t, f)
			response = importHTTP(f, importBase(f)+"/import/apply", test.token, test.worker, "wrong-binding", importJSON(t, ImportApplyRequest{original.PreviewSHA256, req}))
			requireImportResponse(t, response, 409, "import_preview_mismatch")
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("scope mismatch wrote state")
			}
		})
	}
	otherInstance := newAPIFixture(t)
	// Give both installations the same principal/project selectors so this
	// comparison cannot pass merely because bootstrap principals differ.
	if _, err := otherInstance.store.DB().Exec(`UPDATE credentials SET principal_id=? WHERE is_admin=1`, importPrincipal(t, f)); err != nil {
		t.Fatal(err)
	}
	if got := previewFor(t, otherInstance, req); got.PayloadSHA256 != original.PayloadSHA256 || got.PreviewSHA256 == original.PreviewSHA256 {
		t.Fatal("instance was not bound independently of payload")
	}
	requireImportResponse(t, importHTTP(otherInstance, importBase(otherInstance)+"/import/apply", otherInstance.admin, "", "wrong-instance-binding", importJSON(t, ImportApplyRequest{original.PreviewSHA256, req})), 409, "import_preview_mismatch")
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('other-import-project','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	other := cloneImport(t, req)
	other.RepositoryID = 702
	requireImportResponse(t, importHTTP(f, "/api/v1/projects/other-import-project/reviews/import/apply", f.admin, "", "wrong-project-binding", importJSON(t, ImportApplyRequest{original.PreviewSHA256, other})), 409, "import_preview_mismatch")
	for _, change := range []func(*ObservationRequest){
		func(r *ObservationRequest) { r.Queue[0].Title += " changed" },
		func(r *ObservationRequest) { r.Queue[0], r.Queue[1] = r.Queue[1], r.Queue[0] },
		func(r *ObservationRequest) { r.ObservedAt = "2026-01-02T00:00:00.000Z" },
	} {
		changed := cloneImport(t, req)
		change(&changed)
		if got := previewFor(t, f, changed); got.PreviewSHA256 == original.PreviewSHA256 || got.PayloadSHA256 == original.PayloadSHA256 {
			t.Fatal("meaningful source spelling/order/content change preserved digest")
		}
	}
}

func TestReviewImportSharesReceiptsBothDirectionsAfterAdvanceAndRestart(t *testing.T) {
	f := newAPIFixture(t)
	base := importBase(f)
	first := fullImportRequest()
	p1 := previewFor(t, f, first)
	changed := cloneImport(t, first)
	changed.Source = "different-source"
	changedPreview := previewFor(t, f, changed)
	firstApply := ImportApplyRequest{p1.PreviewSHA256, first}
	created := importHTTP(f, base+"/import/apply", f.admin, "", "shared-first", importJSON(t, firstApply))
	requireImportResponse(t, created, 201, "")
	if direct := f.call("POST", base+"/observations", f.admin, "shared-first", first); direct.Code != 201 || direct.Body.String() != created.Body.String() {
		t.Fatal("apply-to-direct receipt replay changed")
	}
	second := cloneImport(t, first)
	*second.ExpectedRevision = 1
	second.ObservedAt = "2026-01-03T00:00:00Z"
	second.Queue[0].Title = "Updated summary"
	p2 := previewFor(t, f, second)
	direct := f.call("POST", base+"/observations", f.admin, "shared-second", second)
	requireImportResponse(t, direct, 201, "")
	third := cloneImport(t, second)
	*third.ExpectedRevision = 2
	third.ObservedAt = "2026-01-04T00:00:00Z"
	f.observe(t, third, "advance")
	before := importDatabaseState(t, f)
	var seq int
	var dbName, dbPath string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(context.Background(), filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	f.store = store
	mux := http.NewServeMux()
	Register(mux, store)
	f.mux = mux
	for _, test := range []struct {
		key  string
		req  ImportApplyRequest
		want string
	}{{"shared-first", firstApply, created.Body.String()}, {"shared-second", ImportApplyRequest{p2.PreviewSHA256, second}, direct.Body.String()}} {
		result := importHTTP(f, base+"/import/apply", f.admin, "", test.key, importJSON(t, test.req))
		requireImportResponse(t, result, 201, "")
		if result.Body.String() != test.want {
			t.Fatal("replay after advance/restart changed original receipt")
		}
	}
	bad := importHTTP(f, base+"/import/apply", f.admin, "", "shared-first", importJSON(t, ImportApplyRequest{changedPreview.PreviewSHA256, changed}))
	requireImportResponse(t, bad, 409, "idempotency_conflict")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("replay/conflict changed state")
	}
	// Replays remain subject to current administrative access.
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, fmt.Sprintf("%x", sha256.Sum256([]byte(f.admin)))); err != nil {
		t.Fatal(err)
	}
	requireImportResponse(t, importHTTP(f, base+"/import/apply", f.admin, "", "shared-first", importJSON(t, firstApply)), 401, "unauthorized")
}

func TestReviewImportAdmissionMatchesOriginalAndRechecksMovement(t *testing.T) {
	f := newAPIFixture(t)
	first := fullImportRequest()
	f.observe(t, first, "original")
	pageResponse := f.call("GET", importBase(f)+"/queue?limit=1", f.readToken, "", nil)
	requireImportResponse(t, pageResponse, 200, "")
	var oldPage QueueResponse
	if err := json.Unmarshal(pageResponse.Body.Bytes(), &oldPage); err != nil || !oldPage.HasMore || oldPage.Cursor == "" {
		t.Fatalf("old snapshot pagination fixture: %v", err)
	}
	valid := cloneImport(t, first)
	*valid.ExpectedRevision = 1
	valid.ObservedAt = "2026-01-03T00:00:00Z"
	for _, test := range []struct {
		name   string
		change func(*ObservationRequest)
		status int
		code   string
	}{
		{"repository", func(r *ObservationRequest) { r.RepositoryID++ }, 400, "validation_failed"},
		{"revision", func(r *ObservationRequest) { *r.ExpectedRevision = 0 }, 409, "revision_conflict"},
		{"observation time", func(r *ObservationRequest) { r.ObservedAt = "2026-01-01T00:00:00Z" }, 409, "stale_observation"},
		{"retirement", func(r *ObservationRequest) { r.Queue = []PullInput{} }, 400, "validation_failed"},
		{"finalized counts", func(r *ObservationRequest) { r.Queue[0].Runs[0].Accepted = 0 }, 409, "observation_conflict"},
		{"finalized finding", func(r *ObservationRequest) { r.Queue[0].Runs[0].Findings[0].Title = "Changed source text" }, 409, "observation_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := cloneImport(t, valid)
			test.change(&req)
			before := importDatabaseState(t, f)
			base := importBase(f)
			requireImportResponse(t, f.call("POST", base+"/import/preview", f.admin, "", req), test.status, test.code)
			requireImportResponse(t, f.call("POST", base+"/observations", f.admin, "refused-original", req), test.status, test.code)
			// The binding is stateless, not an authorization/CAS approval token.
			body, err := normalizeImport(&req)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := importPreview(f.store.InstanceID(), core.Scope{ProjectID: f.project, PrincipalID: importPrincipal(t, f)}, req, body)
			if err != nil {
				t.Fatal(err)
			}
			requireImportResponse(t, f.call("POST", base+"/import/apply", f.admin, "refused-guided", ImportApplyRequest{binding.PreviewSHA256, req}), test.status, test.code)
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("refusal changed state")
			}
		})
	}
	preview := previewFor(t, f, valid)
	f.observe(t, valid, "intervening")
	before := importDatabaseState(t, f)
	requireImportResponse(t, f.call("POST", importBase(f)+"/import/apply", f.admin, "stale-preview", ImportApplyRequest{preview.PreviewSHA256, valid}), 409, "revision_conflict")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("stale preview bypassed CAS")
	}
	// Explicit retirements remain supported and historical snapshots survive.
	retired := reviewRequest(2, []PullInput{})
	retired.Retired = []RetiredPull{{7, "superseded"}, {8, "defunct"}}
	p := previewFor(t, f, retired)
	if p.Counts != (ImportCounts{Retirements: 2}) {
		t.Fatal("retirement preview incorrect")
	}
	requireImportResponse(t, f.call("POST", importBase(f)+"/import/apply", f.admin, "retire", ImportApplyRequest{p.PreviewSHA256, retired}), 201, "")
	if countRows(t, f.store, "review_snapshots") != 3 || countRows(t, f.store, "review_retired_pulls") != 2 {
		t.Fatal("retirement lost history")
	}
	continued := f.call("GET", importBase(f)+"/queue?limit=1&cursor="+url.QueryEscape(oldPage.Cursor), f.readToken, "", nil)
	requireImportResponse(t, continued, 200, "")
	var historyPage QueueResponse
	if err := json.Unmarshal(continued.Body.Bytes(), &historyPage); err != nil || historyPage.SnapshotID != oldPage.SnapshotID || historyPage.Revision != oldPage.Revision || len(historyPage.Items) != 1 || historyPage.Items[0].Number != 8 {
		t.Fatalf("retirement changed old snapshot continuation: %v %+v", err, historyPage)
	}
}

func importPrincipal(t *testing.T, f *apiFixture) string {
	t.Helper()
	var principal string
	sum := sha256.Sum256([]byte(f.admin))
	if err := f.store.DB().QueryRow(`SELECT principal_id FROM credentials WHERE token_sha256=?`, hex.EncodeToString(sum[:])).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return principal
}

func TestReviewImportSizeBoundaryIsApplyable(t *testing.T) {
	f := newAPIFixture(t)
	req := reviewRequest(0, []PullInput{samplePull(1, "Boundary")})
	run := RunInput{SourceRunID: "large", Channel: "manual", Scope: "full", Outcome: "completed", Complete: true,
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 16}
	for i := 0; i < 16; i++ {
		run.Findings = append(run.Findings, Finding{SourceFindingID: fmt.Sprint(i), Title: "Large history", Detail: strings.Repeat("x", 16<<10), Severity: "unknown", Disposition: "unadjudicated"})
	}
	req.Queue[0].Runs = []RunInput{run}
	if err := validateObservation(&req); err != nil {
		t.Fatal(err)
	}
	initialSize := len(importJSON(t, ImportApplyRequest{strings.Repeat("0", 64), req}))
	excess := initialSize - maxObservationBytes
	if excess < 1 || excess >= 16<<10 {
		t.Fatalf("bad boundary fixture: excess=%d", excess)
	}
	req.Queue[0].Runs[0].Findings[15].Detail = strings.Repeat("x", (16<<10)-excess)
	if got := len(importJSON(t, ImportApplyRequest{strings.Repeat("0", 64), req})); got != maxObservationBytes {
		t.Fatalf("not exact boundary: %d", got)
	}
	p := previewFor(t, f, req)
	tooLarge := cloneImport(t, req)
	tooLarge.Queue[0].Runs[0].Findings[15].Detail += "x"
	if len(importJSON(t, tooLarge)) >= maxObservationBytes {
		t.Fatal("negative must fit preview body but not apply wrapper")
	}
	before := importDatabaseState(t, f)
	requireImportResponse(t, f.call("POST", importBase(f)+"/import/preview", f.admin, "", tooLarge), 400, "validation_failed")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("oversize preview wrote")
	}
	requireImportResponse(t, f.call("POST", importBase(f)+"/import/apply", f.admin, "exact-boundary", ImportApplyRequest{p.PreviewSHA256, req}), 201, "")
	var hash string
	if err := f.store.DB().QueryRow(`SELECT payload_sha256 FROM review_snapshots`).Scan(&hash); err != nil || hash != p.PayloadSHA256 {
		t.Fatalf("stored/preview payload mismatch: %v", err)
	}
}

func TestReviewImportStrictShapesAuthorizationAndAtomicRollback(t *testing.T) {
	f := newAPIFixture(t)
	req := fullImportRequest()
	p := previewFor(t, f, req)
	base := importBase(f)
	before := importDatabaseState(t, f)
	for _, path := range []string{"/import/preview", "/import/apply"} {
		body := importJSON(t, req)
		if path == "/import/apply" {
			body = importJSON(t, ImportApplyRequest{p.PreviewSHA256, req})
		}
		requireImportResponse(t, importHTTP(f, base+path, f.readToken, "", "readonly", body), 403, "forbidden")
	}
	validJSON := string(importJSON(t, req))
	validApply := string(importJSON(t, ImportApplyRequest{p.PreviewSHA256, req}))
	for _, test := range []struct{ path, body string }{
		{"/import/preview", `{"unknown":true,` + validJSON[1:]},
		{"/import/preview", strings.Replace(validJSON, `"source":"test-import"`, `"source":"test-import","source":"test-import"`, 1)},
		{"/import/preview", strings.Replace(validJSON, `"source_run_id":"source-run-7"`, `"source_run_id":"source-run-7","unknown":true`, 1)},
		{"/import/preview", strings.Replace(validJSON, `"title":"PRIVATE_FINDING_TITLE"`, `"title":"\ud800"`, 1)},
		{"/import/apply", `{"unknown":true,` + validApply[1:]},
		{"/import/preview", `{"expected_revision":0,"expected_revision":0}`},
		{"/import/preview", `{"expected_revision":null}`},
		{"/import/preview", `{"expected_revision":0,"extra":true}`},
		{"/import/preview", `{"expected_revision":0,"source":"\ud800"}`},
		{"/import/apply", `null`},
		{"/import/apply", `{"preview_sha256":null,"observation":{}}`},
		{"/import/apply", `{"preview_sha256":"` + strings.Repeat("A", 64) + `","observation":{}}`},
		{"/import/apply", `{"preview_sha256":"` + p.PreviewSHA256 + `","observation":null}`},
		{"/import/apply", `{"preview_sha256":"` + p.PreviewSHA256 + `","observation":{},"unknown":true}`},
	} {
		requireImportResponse(t, importHTTP(f, base+test.path, f.admin, "", "malformed", []byte(test.body)), 400, "validation_failed")
	}
	requireImportResponse(t, f.call("POST", base+"/import/preview?extra=1", f.admin, "", req), 400, "validation_failed")
	changed := cloneImport(t, req)
	changed.Queue[0].Title = "Changed after preview"
	requireImportResponse(t, f.call("POST", base+"/import/apply", f.admin, "mismatch", ImportApplyRequest{p.PreviewSHA256, changed}), 409, "import_preview_mismatch")
	requireImportResponse(t, f.call("POST", base+"/import/apply", f.admin, "", ImportApplyRequest{p.PreviewSHA256, req}), 400, "validation_failed")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("invalid requests changed state")
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER import_event_failure BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'PRIVATE_IMPORT_SQL'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.call("POST", base+"/import/apply", f.admin, "rollback", ImportApplyRequest{p.PreviewSHA256, req})
	requireImportResponse(t, failed, 500, "storage_unavailable")
	if strings.Contains(failed.Body.String(), "PRIVATE_IMPORT_SQL") || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("apply failure exposed SQL or partially wrote")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER import_event_failure`); err != nil {
		t.Fatal(err)
	}
	requireImportResponse(t, f.call("POST", base+"/import/apply", f.admin, "rollback", ImportApplyRequest{p.PreviewSHA256, req}), 201, "")
}

func TestReviewImportConcurrentKeysAndCancelledPreview(t *testing.T) {
	f := newAPIFixture(t)
	req := fullImportRequest()
	preview := previewFor(t, f, req)
	body := importJSON(t, ImportApplyRequest{preview.PreviewSHA256, req})
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- importHTTP(f, importBase(f)+"/import/apply", f.admin, "", "concurrent", body)
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	var first string
	for response := range responses {
		requireImportResponse(t, response, 201, "")
		if first != "" && first != response.Body.String() {
			t.Fatal("equal concurrent apply changed receipt")
		}
		first = response.Body.String()
	}
	if countRows(t, f.store, "review_snapshots") != 1 || countRows(t, f.store, "receipts") != 1 || countRows(t, f.store, "events") != 1 {
		t.Fatal("concurrent apply duplicated work")
	}
	before := importDatabaseState(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", importBase(f)+"/import/preview", strings.NewReader(string(importJSON(t, req)))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code == 200 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("cancelled preview succeeded or changed state")
	}
}

func TestReviewImportApplyRechecksRepositoryAfterPreview(t *testing.T) {
	f := newAPIFixture(t)
	req := fullImportRequest()
	preview := previewFor(t, f, req)
	if _, err := f.store.DB().Exec(`UPDATE projects SET github_repository_id=702 WHERE id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	before := importDatabaseState(t, f)
	requireImportResponse(t, f.call("POST", importBase(f)+"/import/apply", f.admin, "repository-moved", ImportApplyRequest{preview.PreviewSHA256, req}), 400, "validation_failed")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("preview bypassed changed repository binding")
	}
}
