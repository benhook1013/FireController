package projectmap_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/server"
)

type fixture struct {
	t                                                 *testing.T
	store                                             *core.Store
	mux                                               *http.ServeMux
	dir, admin, project, other, token, reader, worker string
}

func setup(t *testing.T) *fixture {
	return setupWithMapSchema(t, projectmap.EnsureSchema)
}
func setupWithMapSchema(t *testing.T, initialize func(context.Context, *core.Store) error) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err = initialize(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir, store: store, admin: strings.TrimSpace(string(token))}
	f.mount()
	f.project = f.newProject("one")
	f.other = f.newProject("two")
	f.token = f.grant("writer", "read", "write")
	f.reader = f.grant("reader", "read")
	w := f.call("POST", "/api/v1/projects/"+f.project+"/workers", f.admin, "worker", map[string]string{"name": "worker"})
	expect(t, w, 200)
	var v struct {
		ID string `json:"id"`
	}
	decode(t, w, &v)
	f.worker = v.ID
	return f
}
func (f *fixture) mount() {
	mux := http.NewServeMux()
	mux.Handle("/", server.New(f.store, server.WithRepositoryResolver(func(_ context.Context, name string) (server.RepositoryIdentity, error) {
		id := int64(1)
		if strings.HasSuffix(name, "two") {
			id = 2
		}
		return server.RepositoryIdentity{ID: id, FullName: name}, nil
	})))
	jobs.Register(mux, f.store)
	projectmap.Register(mux, f.store)
	f.mux = mux
}
func (f *fixture) call(method, path, token, key string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return f.raw(method, path, token, key, raw)
}
func (f *fixture) raw(method, path, token, key string, raw []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if f.worker != "" {
		r.Header.Set("X-FC-Worker", f.worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}
func (f *fixture) newProject(name string) string {
	w := f.call("POST", "/api/v1/projects", f.admin, "project-"+name, map[string]string{"name": name, "repository": "owner/" + name})
	expect(f.t, w, 200)
	var v struct {
		ID string `json:"id"`
	}
	decode(f.t, w, &v)
	return v.ID
}
func (f *fixture) grant(token string, permissions ...string) string {
	sum := sha256.Sum256([]byte(token))
	w := f.call("POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, "grant-"+token, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions})
	expect(f.t, w, 200)
	return token
}
func (f *fixture) base() string { return "/api/v1/projects/" + f.project + "/map/entries" }
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status%d want%d: %s", w.Code, status, w.Body.String())
	}
}
func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatal(err)
	}
}
func workstream() projectmap.Editable {
	return projectmap.Editable{Kind: "workstream", Title: "  Named stream  ", Description: "private description\n\t<literal>", State: "editorial custom state", Where: "where", Next: "next", Phases: []projectmap.Phase{{ID: "phase_one", Name: "Phase One", State: "in progress"}, {ID: "phase_two", Name: "Phase Two"}}, Links: []projectmap.Link{{Label: "reference", URL: "https://example.test/private?q=1&x=2"}}}
}
func (f *fixture) create(key string, v projectmap.Editable) projectmap.Receipt {
	w := f.call("POST", f.base(), f.token, key, v)
	expect(f.t, w, 201)
	var r projectmap.Receipt
	decode(f.t, w, &r)
	return r
}
func (f *fixture) read(id string) projectmap.Entry {
	w := f.call("GET", f.base()+"/"+id, f.reader, "", nil)
	expect(f.t, w, 200)
	var e projectmap.Entry
	decode(f.t, w, &e)
	return e
}
func (f *fixture) replace(id, key string, rev int64, v projectmap.Editable) *httptest.ResponseRecorder {
	return f.call("PUT", f.base()+"/"+id, f.token, key, map[string]any{"expected_revision": rev, "entry": v})
}
func (f *fixture) counts() []int {
	var out []int
	for _, table := range []string{"project_map_entries", "project_map_entry_history", "job_workstreams", "job_workstream_history", "events", "receipts"} {
		var n int
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}
func (f *fixture) validate() error {
	tx, err := f.store.DB().BeginTx(f.t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	return projectmap.ValidateSnapshot(f.t.Context(), tx)
}

func TestMapLifecycleStablePhasesReplayAndPrivacy(t *testing.T) {
	f := setup(t)
	v := workstream()
	created := f.create("create", v)
	if created.Revision != 1 || created.Outcome != "created" || !strings.HasPrefix(created.ID, "map_entry_") {
		t.Fatal(created)
	}
	initial := f.read(created.ID)
	if initial.Title != "Named stream" || initial.ProjectID != f.project || initial.Description != v.Description {
		t.Fatal(initial)
	}
	// Nil and explicit empty collections normalize to the same content and key.
	empty := projectmap.Editable{Kind: "return_point", Title: "deferred", Trigger: "when appropriate"}
	r := f.create("empty", empty)
	empty.Phases = []projectmap.Phase{}
	empty.Links = []projectmap.Link{}
	retry := f.create("empty", empty)
	if r != retry {
		t.Fatal(retry, r)
	}
	e := f.read(r.ID)
	if e.Phases == nil || e.Links == nil {
		t.Fatal("null output")
	}
	before := f.counts()
	noop := f.replace(created.ID, "noop", 1, v)
	expect(t, noop, 200)
	var nr projectmap.Receipt
	decode(t, noop, &nr)
	if nr.Outcome != "unchanged" || nr.Revision != 1 {
		t.Fatal(nr)
	}
	after := f.counts()
	if !reflect.DeepEqual(before[:5], after[:5]) || after[5] != before[5]+1 {
		t.Fatal(before, after)
	}
	// Renaming, reordering and archiving retain stable phase identities.
	v.Title = "renamed"
	v.Phases = []projectmap.Phase{v.Phases[1], v.Phases[0]}
	v.Phases[1].Name = "Renamed Phase"
	v.Phases[1].Archived = true
	v.Archived = true
	first := f.replace(created.ID, "first", 1, v)
	expect(t, first, 200)
	current := f.read(created.ID)
	if current.Revision != 2 || current.CreatedAt != initial.CreatedAt || current.Phases[1].ID != "phase_one" || !current.Phases[1].Archived {
		t.Fatal(current)
	}
	old := f.call("GET", f.base()+"/"+created.ID+"/history/1", f.reader, "", nil)
	expect(t, old, 200)
	var original projectmap.Revision
	decode(t, old, &original)
	if !reflect.DeepEqual(original.State, initial) || original.CreatedAt != initial.UpdatedAt || original.ActorID == "" {
		t.Fatal(original)
	}
	v.Archived = false
	expect(t, f.replace(created.ID, "restore", 2, v), 200)
	before = f.counts()
	replay := f.replace(created.ID, "first", 1, func() projectmap.Editable { x := v; x.Archived = true; return x }())
	expect(t, replay, 200)
	if replay.Body.String() != first.Body.String() || !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("replay changed effects")
	}
	stale := f.replace(created.ID, "stale", 1, v)
	expect(t, stale, 409)
	if !strings.Contains(stale.Body.String(), `"current_revision":3`) {
		t.Fatal(stale.Body.String())
	}
	omitted := v
	omitted.Phases = omitted.Phases[:1]
	expect(t, f.replace(created.ID, "omit", 3, omitted), 400)
	changedKind := v
	changedKind.Kind = "return_point"
	changedKind.Where = ""
	changedKind.Next = ""
	changedKind.Phases = []projectmap.Phase{}
	expect(t, f.replace(created.ID, "kind", 3, changedKind), 400)
	// History pages are compact, reverse ordered, and bind the selected entry.
	w := f.call("GET", f.base()+"/"+created.ID+"/history?limit=1", f.reader, "", nil)
	expect(t, w, 200)
	if strings.Contains(w.Body.String(), "description") || strings.Contains(w.Body.String(), "state") {
		t.Fatal(w.Body.String())
	}
	var page projectmap.Page[projectmap.HistorySummary]
	decode(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 3 || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	next := f.call("GET", f.base()+"/"+created.ID+"/history?limit=1&cursor="+url.QueryEscape(page.Cursor), f.reader, "", nil)
	expect(t, next, 200)
	decode(t, next, &page)
	if page.Items[0].Revision != 2 {
		t.Fatal(page)
	}
	expect(t, f.call("GET", f.base()+"/"+r.ID+"/history?cursor="+url.QueryEscape(page.Cursor), f.reader, "", nil), 400)
	var events string
	rows, err := f.store.DB().Query(`SELECT payload_json FROM events WHERE event_type LIKE 'map.%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		events += s
	}
	rows.Close()
	if strings.Contains(events, "private") || strings.Contains(events, "Named") || strings.Contains(events, "http") {
		t.Fatal(events)
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	// Actual reopen preserves identity, complete historical snapshots and receipts.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.mount()
	expect(t, f.replace(created.ID, "first", 1, func() projectmap.Editable { x := v; x.Archived = true; return x }()), 200)
	if f.read(created.ID).Revision != 3 {
		t.Fatal("reopen lost state")
	}
}

func TestMapValidationScopeAndReadConservation(t *testing.T) {
	f := setup(t)
	v := workstream()
	created := f.create("create", v)
	before := f.counts()
	cases := map[string]func(*projectmap.Editable){
		"blanktitle": func(v *projectmap.Editable) { v.Title = " \t " }, "titlebytes": func(v *projectmap.Editable) { v.Title = strings.Repeat("雪", 81) }, "statecontrol": func(v *projectmap.Editable) { v.State = "a\nb" }, "C1": func(v *projectmap.Editable) { v.Description = "a\u0085" }, "prose": func(v *projectmap.Editable) { v.Next = strings.Repeat("x", 8193) }, "phaseid": func(v *projectmap.Editable) { v.Phases[0].ID = "A" }, "phaseomitted": func(v *projectmap.Editable) { v.Phases[0].Name = "" }, "duplicate": func(v *projectmap.Editable) { v.Phases[1].ID = v.Phases[0].ID }, "count": func(v *projectmap.Editable) { v.Phases = make([]projectmap.Phase, 65) }, "javascript": func(v *projectmap.Editable) { v.Links[0].URL = "javascript:alert(1)" }, "file": func(v *projectmap.Editable) { v.Links[0].URL = "file:///tmp/f" }, "relative": func(v *projectmap.Editable) { v.Links[0].URL = "//example.test" }, "userinfo": func(v *projectmap.Editable) { v.Links[0].URL = "https://user:secret@example.test" }, "urlspace": func(v *projectmap.Editable) { v.Links[0].URL = "https://example.test/a b" }, "linkcount": func(v *projectmap.Editable) { v.Links = make([]projectmap.Link, 13) }, "trigger": func(v *projectmap.Editable) { v.Trigger = "not allowed" }, "encoded": func(v *projectmap.Editable) {
			v.Description = strings.Repeat("<", 8192)
			v.Where = v.Description
			v.Next = v.Description
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			x := workstream()
			change(&x)
			expect(t, f.call("POST", f.base(), f.token, "bad-"+name, x), 400)
		})
	}
	valid, _ := json.Marshal(v)
	for name, raw := range map[string]string{"unknown": strings.TrimSuffix(string(valid), "}") + `,"secret":1}`, "case": strings.Replace(string(valid), `"kind":`, `"Kind":`, 1), "duplicate": strings.TrimSuffix(string(valid), "}") + `,"title":"duplicate"}`, "nullbool": strings.Replace(string(valid), `"archived":false`, `"archived":null`, 1), "missingtitle": strings.Replace(string(valid), `"title":"  Named stream  ",`, "", 1), "loneunicode": strings.Replace(string(valid), "Named stream", `\ud800`, 1)} {
		t.Run(name, func(t *testing.T) { expect(t, f.raw("POST", f.base(), f.token, "raw-"+name, []byte(raw)), 400) })
	}
	expect(t, f.call("POST", f.base(), f.reader, "forbidden", v), 403)
	expect(t, f.call("GET", f.base(), "", "", nil), 401)
	worker := f.worker
	f.worker = ""
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/map/entries/"+created.ID, f.admin, "", nil), 404) // actor header is scoped too
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/map/entries/"+created.ID, f.token, "", nil), 403)
	f.worker = worker
	expect(t, f.call("GET", f.base()+"/map_entry_00000000000000000000000000000000", f.reader, "", nil), 404)
	expect(t, f.call("GET", f.base()+"?archived=all", f.reader, "", nil), 400)
	expect(t, f.call("GET", f.base()+"?kind=", f.reader, "", nil), 400)
	expect(t, f.call("GET", f.base()+"?kind=workstream&kind=return_point", f.reader, "", nil), 400)
	for _, path := range []string{f.base(), f.base() + "/" + created.ID, f.base() + "/" + created.ID + "/history", f.base() + "/" + created.ID + "/history/1"} {
		expect(t, f.call("GET", path, f.reader, "", nil), 200)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("invalid requests/reads mutated", before, f.counts())
	}
	// Revocation applies even to an exact mutation replay.
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, func() string { x := sha256.Sum256([]byte(f.token)); return hex.EncodeToString(x[:]) }()); err != nil {
		t.Fatal(err)
	}
	expect(t, f.call("POST", f.base(), f.token, "create", v), 401)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("revoked replay effects")
	}
}

func TestMapOptionalInputDefaultsAndWholeReplacement(t *testing.T) {
	f := setup(t)
	simple := []byte(`{"kind":"workstream","title":" minimal ","phases":[{"id":"start","name":" Start "}]}`)
	w := f.raw("POST", f.base(), f.token, "minimal", simple)
	expect(t, w, 201)
	var r projectmap.Receipt
	decode(t, w, &r)
	e := f.read(r.ID)
	if e.Title != "minimal" || e.State != "" || e.Archived || e.Links == nil || e.Phases[0].Name != "Start" || e.Phases[0].Archived || e.Phases[0].State != "" {
		t.Fatal(e)
	}
	v := workstream()
	v.Phases = e.Phases
	expect(t, f.replace(r.ID, "populate", 1, v), 200)
	raw := []byte(`{"expected_revision":2,"entry":{"kind":"workstream","title":"minimal","phases":[{"id":"start","name":"Start"}],"links":null}}`)
	expect(t, f.raw("PUT", f.base()+"/"+r.ID, f.token, "replace-defaults", raw), 200)
	e = f.read(r.ID)
	if e.Description != "" || e.Next != "" || e.Where != "" || e.State != "" || e.Links == nil || len(e.Links) != 0 || e.Revision != 3 {
		t.Fatal(e)
	}
	before := f.counts()
	for _, field := range []string{"kind", "title", "description", "state", "where", "next", "trigger", "archived"} {
		body := map[string]any{"kind": "workstream", "title": "invalid", field: nil}
		expect(t, f.call("POST", f.base(), f.token, "null-"+field, body), 400)
	}
	for _, field := range []string{"id", "name", "state", "archived"} {
		body := map[string]any{"kind": "workstream", "title": "invalid", "phases": []any{map[string]any{"id": "one", "name": "one", field: nil}}}
		expect(t, f.call("POST", f.base(), f.token, "null-phase-"+field, body), 400)
	}
	for _, body := range []string{`{"kind":"workstream"}`, `{"title":"missingkind"}`, `{"kind":"workstream","title":"x","phases":[{"id":"x"}]}`, `{"kind":"workstream","title":"x","phases":[null]}`, `{"kind":"workstream","title":"x","links":[{"label":"x"}]}`} {
		expect(t, f.raw("POST", f.base(), f.token, "invalid-structure", []byte(body)), 400)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("invalid optional null changed storage")
	}
}

func TestMapPaginationCASAndEventRollback(t *testing.T) {
	f := setup(t)
	for i := 0; i < 28; i++ {
		v := workstream()
		v.Title = fmt.Sprintf("stream%d", i)
		f.create(fmt.Sprintf("create%d", i), v)
	}
	first := f.call("GET", f.base(), f.reader, "", nil)
	expect(t, first, 200)
	var p projectmap.Page[projectmap.Summary]
	decode(t, first, &p)
	if len(p.Items) != 25 || !p.HasMore || p.Cursor == "" {
		t.Fatal(p)
	}
	for i := 1; i < len(p.Items); i++ {
		if p.Items[i-1].ID >= p.Items[i].ID {
			t.Fatal("order")
		}
	}
	cursor := p.Cursor
	second := f.call("GET", f.base()+"?cursor="+url.QueryEscape(cursor), f.reader, "", nil)
	expect(t, second, 200)
	p = projectmap.Page[projectmap.Summary]{}
	decode(t, second, &p)
	if len(p.Items) != 3 || p.HasMore || p.Cursor != "" {
		t.Fatal(p)
	}
	expect(t, f.call("GET", f.base()+"?archived=true&cursor="+url.QueryEscape(cursor), f.reader, "", nil), 400)
	expect(t, f.call("GET", f.base()+"?kind=workstream&cursor="+url.QueryEscape(cursor), f.reader, "", nil), 400)
	// Concurrent distinct keys with the same guard permit exactly one change.
	id := p.Items[0].ID
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := workstream()
			v.Title = fmt.Sprint("racer", i)
			w := f.replace(id, fmt.Sprint("race", i), 1, v)
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	total := 0
	for c := range codes {
		if c != 200 && c != 409 {
			t.Fatal(c)
		}
		total += c
	}
	if total != 609 {
		t.Fatal(total)
	}
	before := f.counts()
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_map_event BEFORE INSERT ON events WHEN NEW.event_type LIKE 'map.%' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	v := workstream()
	v.Title = "rolled back"
	expect(t, f.replace(id, "fail", 2, v), 500)
	expect(t, f.call("POST", f.base(), f.token, "fail-create", v), 500)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("event rollback partial effects", before, f.counts())
	}
	if f.read(id).Revision != 2 {
		t.Fatal("failed transition retained")
	}
}

func TestMapBoundsCancellationAndCurrentArchiveSeparation(t *testing.T) {
	f := setup(t)
	v := workstream()
	v.Title = strings.Repeat("&", 240)
	v.State = strings.Repeat("<", 240)
	v.Phases = []projectmap.Phase{}
	for i := 0; i < 64; i++ {
		v.Phases = append(v.Phases, projectmap.Phase{ID: fmt.Sprintf("phase_%d", i), Name: strings.Repeat("n", 160), State: strings.Repeat("s", 240)})
	}
	v.Links = []projectmap.Link{}
	for i := 0; i < 12; i++ {
		v.Links = append(v.Links, projectmap.Link{Label: strings.Repeat("l", 120), URL: "https://example.test/ref"})
	}
	r := f.create("bounded", v)
	e := f.read(r.ID)
	if len(e.Phases) != 64 || len(e.Links) != 12 || e.Title != v.Title || e.State != v.State {
		t.Fatal("valid boundaries changed")
	}
	before := f.counts()
	raw, _ := json.Marshal(v)
	request := httptest.NewRequest("POST", f.base(), bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("Idempotency-Key", "cancelled")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request = request.WithContext(ctx)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, request)
	expect(t, w, 503)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("cancelled mutation changed data")
	}
	request = httptest.NewRequest("GET", f.base(), nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+f.reader)
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, request)
	expect(t, w, 503)
	// Runtime current reads remain local even when an archive is unavailable;
	// operator inspection is the explicit whole-history validation boundary.
	if _, err := f.store.DB().Exec(`DELETE FROM project_map_entry_history WHERE entry_id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if f.read(r.ID).Revision != 1 {
		t.Fatal("current read unexpectedly changed")
	}
	if err := f.validate(); err == nil {
		t.Fatal("operator accepted missing history")
	}
}
