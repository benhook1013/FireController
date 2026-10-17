package projectmap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestMapImportStrictInputAuthorityAndNormalizedWireBounds(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	original, _ := json.Marshal(doc)
	before := f.importState()
	changes := map[string]string{
		"duplicate":                 strings.TrimSuffix(string(original), "}") + `,"version":1}`,
		"unknown":                   strings.TrimSuffix(string(original), "}") + `,"extra":true}`,
		"case_alias":                strings.Replace(string(original), `"version":`, `"Version":`, 1),
		"inference_null":            strings.Replace(string(original), `"inferred":true`, `"inferred":null`, 1),
		"inference_missing":         strings.Replace(string(original), `"inferred":true,`, "", 1),
		"confidence_null":           strings.Replace(string(original), `"confidence":"medium"`, `"confidence":null`, 1),
		"expected_revision_null":    strings.Replace(string(original), `"expected_revision":0`, `"expected_revision":null`, 1),
		"expected_revision_missing": strings.Replace(string(original), `"expected_revision":0,`, "", 1),
		"missing_snapshot":          strings.Replace(string(original), `"snapshot":"snapshot-one",`, "", 1),
		"null_scope":                strings.Replace(string(original), `"scope":"Two deliberately selected sources"`, `"scope":null`, 1),
		"lone_unicode":              strings.Replace(string(original), `Private inferred proposal`, `\ud800`, 1),
	}
	for name, raw := range changes {
		t.Run(name, func(t *testing.T) { expect(t, f.raw("POST", f.importPath()+"/preview", f.admin, "", []byte(raw)), 400) })
	}
	for name, change := range map[string]func(*projectmap.ImportDocument){
		"blank_snapshot":      func(d *projectmap.ImportDocument) { d.Source.Snapshot = " \t " },
		"blank_scope":         func(d *projectmap.ImportDocument) { d.Source.Scope = "\n " },
		"unknown_source_kind": func(d *projectmap.ImportDocument) { d.Source.Kind = "provider" },
		"source_key":          func(d *projectmap.ImportDocument) { d.Source.Key = "unicode-雪" },
		"item_key":            func(d *projectmap.ImportDocument) { d.Entries[0].SourceKey = "bad/key" },
		"confidence":          func(d *projectmap.ImportDocument) { d.Entries[0].Provenance.Confidence = "certain" },
		"uncertainty_size":    func(d *projectmap.ImportDocument) { d.Entries[0].Provenance.Uncertainty = strings.Repeat("a", 4097) },
		"unsafe_reference": func(d *projectmap.ImportDocument) {
			d.Entries[0].Provenance.References = []string{"file:///private/source"}
		},
		"credential_reference": func(d *projectmap.ImportDocument) {
			d.Entries[0].Provenance.References = []string{"https://user:secret@example.invalid/"}
		},
		"reference_count": func(d *projectmap.ImportDocument) { d.Entries[0].Provenance.References = make([]string, 21) },
		"duplicate_item":  func(d *projectmap.ImportDocument) { d.Entries = append(d.Entries, d.Entries[0]) },
		"utc_lower_year":  func(d *projectmap.ImportDocument) { d.Source.CollectedAt = "0001-01-01T00:30:00+01:00" },
		"utc_upper_year":  func(d *projectmap.ImportDocument) { d.Source.CollectedAt = "9999-12-31T23:30:00-01:00" },
	} {
		t.Run(name, func(t *testing.T) {
			d := importDocument()
			change(&d)
			expect(t, f.call("POST", f.importPath()+"/preview", f.admin, "", d), 400)
		})
	}
	for _, token := range []string{"", f.reader, f.token} {
		status := 403
		if token == "" {
			status = 401
		}
		expect(t, f.call("POST", f.importPath()+"/preview", token, "", doc), status)
	}
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("invalid previews or unauthorized inputs changed state")
	}
	// Defaults preserve explicitly recorded inference, while absent confidence
	// and absent/null references remain safe unknown/empty provenance.
	defaultRaw := strings.Replace(string(original), `"confidence":"medium",`, "", 1)
	defaultRaw = strings.Replace(defaultRaw, `"references":["https://example.invalid/source#section"]`, `"references":null`, 1)
	normalized, err := projectmap.DecodeImportDocument([]byte(defaultRaw))
	if err != nil || normalized.Entries[0].Provenance.Confidence != "unknown" || normalized.Entries[0].Provenance.References == nil || !normalized.Entries[0].Provenance.Inferred {
		t.Fatal("safe defaults", err)
	}
	// Raw legal '<' bytes fit, but Go's canonical encoding expands them. The
	// future apply wrapper must fit before any preview can approve it.
	large := importDocument()
	large.Entries = nil
	for n := 0; n < 11; n++ {
		item := doc.Entries[0]
		item.SourceKey = fmt.Sprintf("item-%d", n)
		item.Provenance.Uncertainty = strings.Repeat("<", 4096)
		large.Entries = append(large.Entries, item)
	}
	raw, _ := json.Marshal(large)
	raw = []byte(strings.ReplaceAll(string(raw), `\u003c`, `<`))
	if len(raw) > projectmap.MaxImportBytes {
		t.Fatal("invalid boundary fixture")
	}
	expect(t, f.raw("POST", f.importPath()+"/preview", f.admin, "", raw), 400)
	large.Entries = large.Entries[:9]
	preview := f.previewImport(large)
	if !preview.Ready {
		t.Fatal("nearby valid input rejected")
	}
	if r := f.applyImport(preview, "nearby-valid"); r.Created != 9 {
		t.Fatal(r)
	}
}
func TestMapImportHashBindsActualWorkerPrincipalProjectAndInstance(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	preview := f.previewImport(doc)
	// Create another legitimate identity in each changed scope, so this tests
	// the preview binding instead of merely failing a missing/foreign header.
	w := f.call("POST", "/api/v1/projects/"+f.project+"/workers", f.admin, "other-worker", map[string]string{"name": "other"})
	expect(t, w, 200)
	var otherWorker struct {
		ID string `json:"id"`
	}
	decode(t, w, &otherWorker)
	request := projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256}
	oldWorker := f.worker
	f.worker = otherWorker.ID
	expect(t, f.call("POST", f.importPath(), f.admin, "changed-worker", request), 409)
	f.worker = oldWorker
	sum := sha256.Sum256([]byte(f.token))
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE token_sha256=?`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	expect(t, f.call("POST", f.importPath(), f.token, "changed-principal", request), 409)
	f.worker = ""
	w = f.call("POST", "/api/v1/projects/"+f.other+"/workers", f.admin, "other-project-worker", map[string]string{"name": "other"})
	expect(t, w, 200)
	decode(t, w, &otherWorker)
	f.worker = otherWorker.ID
	expect(t, f.call("POST", "/api/v1/projects/"+f.other+"/map/import", f.admin, "changed-project", request), 409)
	f.worker = oldWorker
	second := setup(t)
	expect(t, second.call("POST", second.importPath(), second.admin, "changed-instance", request), 409)
	// Cookie sessions cannot reach this administrator import boundary, including
	// automatically write-enabled sessions derived from an original administrator.
	w = f.call("POST", "/api/v1/projects/"+f.project+"/browser-session", f.admin, "", map[string]any{"session_id": strings.Repeat("a", 32)})
	expect(t, w, 200)
	var connected struct {
		AllowWrites bool `json:"allow_writes"`
	}
	decode(t, w, &connected)
	if !connected.AllowWrites {
		t.Fatal("administrator cookie must have automatic browser write allowance for this refusal proof")
	}
	raw, _ := json.Marshal(doc)
	r := httptest.NewRequest("POST", f.importPath()+"/preview", strings.NewReader(string(raw)))
	r.Header.Set("X-FC-Browser-Session", strings.Repeat("a", 32))
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", f.worker)
	r.Header.Set("Origin", "http://localhost")
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	out := httptest.NewRecorder()
	f.mux.ServeHTTP(out, r)
	expect(t, out, http.StatusForbidden)
}
func TestMapImportMigratesActualVersionOneWithoutChangingExistingMap(t *testing.T) {
	f := setup(t)
	created := f.create("before-upgrade", workstream())
	before := f.read(created.ID)
	if _, err := f.store.DB().Exec(`DROP TABLE project_map_import_items;DROP TABLE project_map_import_history;DROP TABLE project_map_import_sources;UPDATE metadata SET value='1' WHERE key='schema:project_map'`); err != nil {
		t.Fatal(err)
	}
	if err := f.validate(); err != nil {
		t.Fatal("v1 snapshot invalid", err)
	}
	if err := projectmap.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil || version != "2" {
		t.Fatal("migration version", version, err)
	}
	if !reflect.DeepEqual(before, f.read(created.ID)) {
		t.Fatal("v1 migration changed map data")
	}
	receipt := f.create("before-upgrade", workstream())
	if receipt != created {
		t.Fatal("migration lost old receipt")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	f.applyImport(f.previewImport(importDocument()), "after-upgrade")
	if err := f.validate(); err != nil {
		t.Fatal("new v2 source invalid", err)
	}
}

func TestMapImportConcurrentNewKeysCreateOnlyOneSourceMapping(t *testing.T) {
	f := setup(t)
	preview := f.previewImport(importDocument())
	var wait sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for n := 0; n < 2; n++ {
		wait.Add(1)
		go func(n int) {
			defer wait.Done()
			results <- f.call("POST", f.importPath(), f.admin, fmt.Sprintf("concurrent-import-%d", n), projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256})
		}(n)
	}
	wait.Wait()
	close(results)
	created := 0
	unchanged := 0
	for result := range results {
		expect(t, result, 200)
		var receipt projectmap.ImportReceipt
		decode(t, result, &receipt)
		created += receipt.Created
		unchanged += receipt.Unchanged
		if receipt.Revision != 1 {
			t.Fatal(receipt)
		}
	}
	if created != 1 || unchanged != 1 {
		t.Fatal("concurrent imports duplicated mapped entry")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}
