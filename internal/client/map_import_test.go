package client_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/projectmap"
)

func preparedMapDocument() client.MapImportDocument {
	e := mapEditable()
	if e.Links == nil {
		e.Links = []client.ProjectMapLink{}
	}
	return client.MapImportDocument{Version: 1, Source: client.MapImportSource{Key: "prepared-map", RepositoryID: 1, Kind: "prepared", Snapshot: "selected docs", CollectedAt: "2026-10-17T00:00:00Z", TransformationVersion: 1, Scope: "selected sources only"}, Entries: []client.MapImportItem{{SourceKey: "track-one", Entry: e, Provenance: client.MapImportProvenance{References: []string{"https://example.com/source"}, Inferred: true, Confidence: "medium", Uncertainty: "Proposed structure"}}}}
}
func preparedMapJSON(t *testing.T, d client.MapImportDocument) []byte {
	t.Helper()
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestMapImportClientLocalStrictPreflightAndNormalization(t *testing.T) {
	base := preparedMapJSON(t, preparedMapDocument())
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		write(w, 500, map[string]string{})
	}))
	defer s.Close()
	c := newClient(t, s)
	cases := [][]byte{[]byte(`null`), []byte(`[]`), append(base, []byte(`{}`)...), bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(base, []byte(`"inferred":true`), []byte(`"inferred":null`), 1), bytes.Replace(base, []byte(`"confidence":"medium"`), []byte(`"confidence":null`), 1), bytes.Replace(base, []byte(`"version":1`), []byte(`"Version":1`), 1), bytes.Replace(base, []byte(`"repository_id":1`), []byte(`"repository_id":null`), 1), bytes.Replace(base, []byte(`https://example.com/source`), []byte(`https://user:secret@example.com/source`), 1), bytes.Replace(base, []byte(`"expected_revision":0`), []byte(`"expected_revision":9223372036854775807`), 1)}
	for _, raw := range cases {
		if _, e := c.PreviewMapImport(t.Context(), raw); e == nil {
			t.Fatal("invalid local document accepted", string(raw))
		}
		if _, e := c.ApplyMapImport(t.Context(), "key", strings.Repeat("a", 64), raw); e == nil {
			t.Fatal("invalid local apply accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid document contacted service")
	}
	optional := bytes.Replace(base, []byte(`"confidence":"medium","uncertainty":"Proposed structure"`), nil, 1)
	optional = bytes.Replace(optional, []byte(`"inferred":true,}`), []byte(`"inferred":true}`), 1)
	d, err := client.DecodeMapImportDocument(optional)
	if err != nil || d.Entries[0].Provenance.Confidence != "unknown" || d.Entries[0].Provenance.Uncertainty != "" {
		t.Fatal("omission defaults", err)
	}
	input := preparedMapDocument()
	input.Source.CollectedAt = "2026-10-17T13:00:00+13:00"
	input.Source.Snapshot = " unknown "
	input.Source.Scope = " selected sources "
	input.Entries[0].Entry.Title = "  Build map  "
	input.Entries[0].Provenance.References = nil
	d, err = client.DecodeMapImportDocument(preparedMapJSON(t, input))
	if err != nil || d.Source.CollectedAt != "2026-10-17T00:00:00Z" || d.Entries[0].Entry.Title != "Build map" || d.Entries[0].Provenance.References == nil {
		t.Fatal("canonical normalization", d, err)
	}
	for _, change := range []func(*client.MapImportDocument){func(d *client.MapImportDocument) { d.Source.Snapshot = " \n\t" }, func(d *client.MapImportDocument) { d.Source.Scope = "" }, func(d *client.MapImportDocument) { d.Source.CollectedAt = "0001-01-01T00:30:00+01:00" }, func(d *client.MapImportDocument) { d.Source.CollectedAt = "9999-12-31T23:30:00-01:00" }} {
		bad := preparedMapDocument()
		change(&bad)
		if client.ValidateMapImportDocument(preparedMapJSON(t, bad)) == nil {
			t.Fatal("invalid source boundary accepted", bad.Source)
		}
	}
	large := preparedMapDocument()
	large.Entries = make([]client.MapImportItem, 100)
	for i := range large.Entries {
		large.Entries[i] = preparedMapDocument().Entries[0]
		large.Entries[i].SourceKey = strings.Repeat("a", i+1)
		large.Entries[i].Entry.Description = strings.Repeat("x", 8<<10)
	}
	if client.ValidateMapImportDocument(preparedMapJSON(t, large)) == nil {
		t.Fatal("oversized document accepted")
	}
	// The document alone fits; its required future apply wrapper does not.
	bounded := preparedMapDocument()
	bounded.Entries = make([]client.MapImportItem, 32)
	for i := range bounded.Entries {
		bounded.Entries[i] = preparedMapDocument().Entries[0]
		bounded.Entries[i].SourceKey = strings.Repeat("a", i+1)
		bounded.Entries[i].Entry.Description = strings.Repeat("x", 7000)
		bounded.Entries[i].Provenance.Uncertainty = ""
	}
	for i := range bounded.Entries {
		remaining := (256 << 10) - 1 - len(preparedMapJSON(t, bounded))
		if remaining <= 0 {
			break
		}
		bounded.Entries[i].Provenance.Uncertainty = strings.Repeat("x", min(remaining, 4096))
	}
	boundedRaw := preparedMapJSON(t, bounded)
	if len(boundedRaw) != (256<<10)-1 || client.ValidateMapImportDocument(boundedRaw) == nil {
		t.Fatal("future wrapper budget not enforced", len(boundedRaw))
	}
}

func TestMapImportClientRetainedReadIdentityShapeAndSinglePage(t *testing.T) {
	d := preparedMapDocument()
	record := client.MapImportRecord{SourceKey: d.Source.Key, Revision: 2, Document: d, Mappings: []client.MapImportMapping{{SourceKey: "track-one", EntryID: "map_entry_" + strings.Repeat("a", 32), EntryRevision: 1, SourceRevision: 2}}, ActorID: "principal-one", WorkerID: "worker-one", CreatedAt: "2026-10-17T00:00:00Z"}
	var response atomic.Value
	raw, _ := json.Marshal(record)
	response.Store(string(raw))
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("read changed transport")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response.Load().(string))
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, e := c.MapImport(t.Context(), d.Source.Key); e != nil {
		t.Fatal(e)
	}
	for _, edit := range []func(map[string]any){func(v map[string]any) { v["source_key"] = "other" }, func(v map[string]any) { v["revision"] = 0 }, func(v map[string]any) { v["mappings"] = nil }, func(v map[string]any) { v["mappings"].([]any)[0].(map[string]any)["source_revision"] = 1 }, func(v map[string]any) { delete(v, "worker_id") }, func(v map[string]any) {
		v["document"].(map[string]any)["entries"].([]any)[0].(map[string]any)["entry"].(map[string]any)["title"] = " unnormalized "
	}} {
		var v map[string]any
		json.Unmarshal(raw, &v)
		edit(v)
		b, _ := json.Marshal(v)
		response.Store(string(b))
		_, e := c.MapImport(t.Context(), d.Source.Key)
		checkError(t, e, "invalid_response")
	}
	response.Store(string(raw))
	_, err := c.MapImportRevision(t.Context(), d.Source.Key, 1)
	checkError(t, err, "invalid_response")
	page := client.Page[client.MapImportHistorySummary]{Items: []client.MapImportHistorySummary{{Revision: 2, ActorID: "principal-one", WorkerID: "worker-one", CreatedAt: record.CreatedAt}}, HasMore: true, Cursor: "retained-next"}
	b, _ := json.Marshal(page)
	response.Store(string(b))
	before := calls.Load()
	v, err := c.MapImportHistory(t.Context(), d.Source.Key, client.PageOptions{Limit: 1})
	if err != nil || !v.HasMore || calls.Load() != before+1 {
		t.Fatal("history auto-followed or failed", v, err)
	}
	before = calls.Load()
	if _, e := c.MapImport(t.Context(), "bad/key"); e == nil {
		t.Fatal("unsafe key accepted")
	}
	if _, e := c.MapImportRevision(t.Context(), d.Source.Key, 0); e == nil {
		t.Fatal("invalid revision accepted")
	}
	if calls.Load() != before {
		t.Fatal("bad local reads contacted service")
	}
}

func TestMapImportClientStrictPreviewReceiptsAndBoundPaths(t *testing.T) {
	d := preparedMapDocument()
	raw := preparedMapJSON(t, d)
	preview := client.MapImportPreview{Document: d, Ready: true, PreviewSHA256: strings.Repeat("b", 64), Plan: []client.MapImportPlan{{SourceKey: "track-one", Action: "create", Reason: "new_source_item"}}}
	var response atomic.Value
	data, _ := json.Marshal(preview)
	response.Store(string(data))
	var status atomic.Int64
	status.Store(200)
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("lost binding")
		}
		if r.URL.Path != "/api/v1/projects/project-one/map/import/preview" && r.URL.Path != "/api/v1/projects/project-one/map/import" {
			t.Error("wrong route", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "preview") && r.Header.Get("Idempotency-Key") != "" {
			t.Error("preview mutation key")
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		io.WriteString(w, response.Load().(string))
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, e := c.PreviewMapImport(t.Context(), raw); e != nil {
		t.Fatal(e)
	}
	for _, edit := range []func(map[string]any){func(v map[string]any) { delete(v, "ready") }, func(v map[string]any) { v["ready"] = nil }, func(v map[string]any) { v["ready"] = false }, func(v map[string]any) { v["conflicts"] = 1 }, func(v map[string]any) { v["preview_sha256"] = "" }, func(v map[string]any) { v["extra"] = true }, func(v map[string]any) { v["plan"].([]any)[0].(map[string]any)["source_key"] = "other" }, func(v map[string]any) { v["document"].(map[string]any)["source"].(map[string]any)["repository_id"] = 2 }} {
		var v map[string]any
		json.Unmarshal(data, &v)
		edit(v)
		b, _ := json.Marshal(v)
		response.Store(string(b))
		_, e := c.PreviewMapImport(t.Context(), raw)
		checkError(t, e, "invalid_response")
	}
	response.Store(strings.TrimSuffix(string(data), "}") + `,"ready":true}`)
	_, err := c.PreviewMapImport(t.Context(), raw)
	checkError(t, err, "invalid_response")
	conflicted := preview
	conflicted.Ready = false
	conflicted.PreviewSHA256 = ""
	conflicted.Conflicts = 1
	conflicted.Plan = []client.MapImportPlan{{SourceKey: "track-one", Action: "conflict", Reason: "unmapped_source_item"}}
	b, _ := json.Marshal(conflicted)
	response.Store(string(b))
	if v, e := c.PreviewMapImport(t.Context(), raw); e != nil || v.Ready {
		t.Fatal("valid conflict preview", v, e)
	}
	response.Store(`{"source_key":"other","revision":1,"outcome":"imported","created":1,"updated":0,"unchanged":0}`)
	before := calls.Load()
	_, err = c.ApplyMapImport(t.Context(), "exact-key", strings.Repeat("b", 64), raw)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != before+1 {
		t.Fatal("bad receipt not unknown/no retry", err)
	}
	response.Store(`{"source_key":"prepared-map","revision":1,"outcome":"imported","created":1,"updated":0,"unchanged":0}`)
	if v, e := c.ApplyMapImport(t.Context(), "exact-key", strings.Repeat("b", 64), raw); e != nil || v.Created != 1 {
		t.Fatal(v, e)
	}
	response.Store(`{"error":{"code":"revision_conflict","message":"safe","details":{"current_revision":2}},"request_id":"request-one"}`)
	status.Store(409)
	_, err = c.ApplyMapImport(t.Context(), "exact-key", strings.Repeat("b", 64), raw)
	if !errors.As(err, &failure) || failure.OutcomeUnknown {
		t.Fatal("known refusal marked uncertain", err)
	}
}

func TestMapImportClientActualServiceReplayHistoryAndManualConflict(t *testing.T) {
	f := newMutationFixture(t)
	if e := projectmap.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	doc := preparedMapDocument()
	raw := preparedMapJSON(t, doc)
	provider := f.providerCalls.Load()
	preview, err := c.PreviewMapImport(t.Context(), raw)
	if err != nil || !preview.Ready {
		t.Fatal(preview, err)
	}
	receipt, err := c.ApplyMapImport(t.Context(), "map-import-original", preview.PreviewSHA256, raw)
	if err != nil || receipt.Created != 1 || receipt.Revision != 1 {
		t.Fatal(receipt, err)
	}
	noop, err := c.ApplyMapImport(t.Context(), "map-import-deliberate-replay", preview.PreviewSHA256, raw)
	if err != nil || noop.Outcome != "unchanged" || noop.Unchanged != 1 || noop.Revision != 1 {
		t.Fatal("new key duplicated import", noop, err)
	}
	current, err := c.MapImport(t.Context(), doc.Source.Key)
	if err != nil || current.Revision != 1 || len(current.Mappings) != 1 || !reflect.DeepEqual(current.Document, doc) {
		t.Fatal(current, err)
	}
	history, err := c.MapImportHistory(t.Context(), doc.Source.Key, client.PageOptions{Limit: 1})
	if err != nil || len(history.Items) != 1 || history.HasMore {
		t.Fatal(history, err)
	}
	frozen, err := c.MapImportRevision(t.Context(), doc.Source.Key, 1)
	if err != nil || !reflect.DeepEqual(frozen, current) {
		t.Fatal(frozen, err)
	}
	entry, err := c.ProjectMapEntry(t.Context(), current.Mappings[0].EntryID)
	if err != nil {
		t.Fatal(err)
	}
	editable := doc.Entries[0].Entry
	editable.Title = "Manual change"
	if _, err = c.ReplaceProjectMapEntry(t.Context(), "manual-map-edit", entry.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: entry.Revision, Entry: editable}); err != nil {
		t.Fatal(err)
	}
	replayed, err := c.ApplyMapImport(t.Context(), "map-import-original", preview.PreviewSHA256, raw)
	if err != nil || replayed != receipt {
		t.Fatal("original receipt not retained", replayed, err)
	}
	conflict, err := c.PreviewMapImport(t.Context(), raw)
	if err != nil || conflict.Ready || conflict.Conflicts != 1 || conflict.PreviewSHA256 != "" {
		t.Fatal("manual edit lost", conflict, err)
	}
	if f.providerCalls.Load() != provider {
		t.Fatal("map import called provider")
	}
}

func TestMapImportClientLargePlanRefusalRemainsKnown(t *testing.T) {
	f := newMutationFixture(t)
	if e := projectmap.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	doc := preparedMapDocument()
	doc.Entries = make([]client.MapImportItem, 100)
	for i := range doc.Entries {
		doc.Entries[i] = preparedMapDocument().Entries[0]
		doc.Entries[i].SourceKey = strings.Repeat("a", i+1)
	}
	raw := preparedMapJSON(t, doc)
	preview, err := c.PreviewMapImport(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApplyMapImport(t.Context(), "large-map", preview.PreviewSHA256, raw); err != nil {
		t.Fatal(err)
	}
	record, err := c.MapImport(t.Context(), doc.Source.Key)
	if err != nil {
		t.Fatal(err)
	}
	entry := doc.Entries[0].Entry
	entry.Title = "Manual update"
	if _, err = c.ReplaceProjectMapEntry(t.Context(), "manual-change", record.Mappings[0].EntryID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: entry}); err != nil {
		t.Fatal(err)
	}
	_, err = c.ApplyMapImport(t.Context(), "large-map-new-key", preview.PreviewSHA256, raw)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "map_import_conflict" || failure.OutcomeUnknown || failure.Status != 409 || !strings.Contains(failure.Action, "Preview") {
		t.Fatal("bounded known refusal lost", err)
	}
}

func TestMapImportClientPositiveRevisionDocumentRetryAndStaleRefusals(t *testing.T) {
	f := newMutationFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	doc := preparedMapDocument()
	raw := preparedMapJSON(t, doc)
	preview, err := c.PreviewMapImport(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApplyMapImport(t.Context(), "initial-map-source", preview.PreviewSHA256, raw); err != nil {
		t.Fatal(err)
	}
	doc.Source.Snapshot = "second source version"
	doc.Entries[0].ExpectedRevision = 1
	doc.Entries[0].Entry.Title = "Approved source update"
	raw = preparedMapJSON(t, doc)
	preview, err = c.PreviewMapImport(t.Context(), raw)
	if err != nil || !preview.Ready || preview.Plan[0].Action != "update" {
		t.Fatal(preview, err)
	}
	updated, err := c.ApplyMapImport(t.Context(), "updated-map-source", preview.PreviewSHA256, raw)
	if err != nil || updated.Revision != 2 || updated.Updated != 1 {
		t.Fatal(updated, err)
	}
	retryPreview, err := c.PreviewMapImport(t.Context(), raw)
	if err != nil || !retryPreview.Ready || retryPreview.Plan[0].Action != "unchanged" || retryPreview.Plan[0].Reason != "retained_document_unchanged" || retryPreview.Plan[0].CurrentRevision != 2 {
		t.Fatal("exact retained document could not preview", retryPreview, err)
	}
	retry, err := c.ApplyMapImport(t.Context(), "deliberate-exact-new-key", retryPreview.PreviewSHA256, raw)
	if err != nil || retry.Outcome != "unchanged" || retry.Revision != 2 || retry.Unchanged != 1 {
		t.Fatal("new key changed retained document", retry, err)
	}
	record, err := c.MapImport(t.Context(), doc.Source.Key)
	if err != nil || record.Revision != 2 {
		t.Fatal(record, err)
	}
	stale := doc
	stale.Entries = append([]client.MapImportItem{}, doc.Entries...)
	stale.Entries[0].Entry.Title = "Different stale source"
	staleRaw := preparedMapJSON(t, stale)
	conflict, err := c.PreviewMapImport(t.Context(), staleRaw)
	if err != nil || conflict.Ready || conflict.Conflicts != 1 {
		t.Fatal("stale changed document admitted", conflict, err)
	}
	_, err = c.ApplyMapImport(t.Context(), "stale-changed-document", retryPreview.PreviewSHA256, staleRaw)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "import_preview_mismatch" || failure.OutcomeUnknown {
		t.Fatal("stale changed apply", err)
	}
	manual := doc.Entries[0].Entry
	manual.Title = "Manual intervening change"
	id := record.Mappings[0].EntryID
	if _, err = c.ReplaceProjectMapEntry(t.Context(), "manual-after-import", id, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 2, Entry: manual}); err != nil {
		t.Fatal(err)
	}
	conflict, err = c.PreviewMapImport(t.Context(), raw)
	if err != nil || conflict.Ready || conflict.Conflicts != 1 {
		t.Fatal("retained retry overwrote manual change", conflict, err)
	}
	_, err = c.ApplyMapImport(t.Context(), "manual-conflict-new-key", retryPreview.PreviewSHA256, raw)
	if !errors.As(err, &failure) || failure.Code != "map_import_conflict" || failure.OutcomeUnknown {
		t.Fatal("manual change not refused", err)
	}
	if _, err = c.ReplaceProjectMapEntry(t.Context(), "manual-revert", id, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 3, Entry: doc.Entries[0].Entry}); err != nil {
		t.Fatal(err)
	}
	conflict, err = c.PreviewMapImport(t.Context(), raw)
	if err != nil || conflict.Ready || conflict.Conflicts != 1 {
		t.Fatal("manual edit/revert erased revision protection", conflict, err)
	}
}

func TestMapImportClientRetainedReasonNeverWidensUpdateOrOtherStalePlan(t *testing.T) {
	doc := preparedMapDocument()
	doc.Entries[0].ExpectedRevision = 1
	var response atomic.Value
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response.Load().(string))
	}))
	defer s.Close()
	c := newClient(t, s)
	for _, test := range []struct {
		action, reason string
		revision       int64
		valid          bool
	}{{"unchanged", "retained_document_unchanged", 2, true}, {"unchanged", "content_unchanged", 2, false}, {"update", "retained_document_unchanged", 2, false}, {"unchanged", "retained_document_unchanged", 1, false}, {"unchanged", "retained_document_unchanged", 0, false}} {
		v := client.MapImportPreview{Document: doc, Ready: true, PreviewSHA256: strings.Repeat("b", 64), Plan: []client.MapImportPlan{{SourceKey: "track-one", Action: test.action, Reason: test.reason, EntryID: "map_entry_" + strings.Repeat("a", 32), CurrentRevision: test.revision}}}
		b, _ := json.Marshal(v)
		response.Store(string(b))
		_, err := c.PreviewMapImport(t.Context(), preparedMapJSON(t, doc))
		if (err == nil) != test.valid {
			t.Fatal(test, err)
		}
	}
}
