package presentation_test

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
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/server"
)

type fixture struct {
	t                                                 *testing.T
	store                                             *core.Store
	mux                                               *http.ServeMux
	dir, admin, project, other, token, reader, worker string
}

func setup(t *testing.T) *fixture {
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
	if err = presentation.EnsureSchema(t.Context(), store); err != nil {
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
	presentation.Register(mux, f.store)
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
func (f *fixture) base() string { return "/api/v1/projects/" + f.project + "/presentation" }
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
func (f *fixture) replace(key string, rev int64, name string) *httptest.ResponseRecorder {
	return f.call("PUT", f.base(), f.admin, key, map[string]any{"expected_revision": rev, "presentation": map[string]any{"name": name}})
}
func (f *fixture) read() presentation.Snapshot {
	w := f.call("GET", f.base(), f.reader, "", nil)
	expect(f.t, w, 200)
	var s presentation.Snapshot
	decode(f.t, w, &s)
	return s
}
func (f *fixture) counts() []int64 {
	out := make([]int64, 4)
	for i, table := range []string{"project_presentations", "project_presentation_history", "events", "receipts"} {
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return out
}
func (f *fixture) validate() error {
	tx, e := f.store.DB().BeginTx(f.t.Context(), &sql.TxOptions{ReadOnly: true})
	if e != nil {
		f.t.Fatal(e)
	}
	defer tx.Rollback()
	return presentation.ValidateSnapshot(f.t.Context(), tx)
}
func TestPresentationLifecycleReplayHistoryAndRestart(t *testing.T) {
	f := setup(t)
	before := f.counts()
	s := f.read()
	if s.Presentation.Revision != 0 || s.Presentation.Name != "one" || s.Presentation.UpdatedAt != "" || s.EventCursor == "" {
		t.Fatalf("implicit %#v", s)
	}
	if got := f.counts(); !reflect.DeepEqual(before, got) {
		t.Fatal("read allocated", before, got)
	}
	// A default replacement is a receipt-only no-op at revision zero.
	w := f.replace("noop-zero", 0, "one")
	expect(t, w, 200)
	if !strings.Contains(w.Body.String(), `"outcome":"unchanged"`) {
		t.Fatal(w.Body.String())
	}
	after := f.counts()
	if after[0] != 0 || after[1] != 0 || after[2] != before[2] || after[3] != before[3]+1 {
		t.Fatal("noop effects", after)
	}
	initialCursor := s.EventCursor
	first := f.replace("first", 0, "  Renamed <literal>  ")
	expect(t, first, 200)
	firstReceipt := first.Body.String()
	s = f.read()
	if s.Presentation.Revision != 1 || s.Presentation.Name != "Renamed <literal>" || s.EventCursor == "" || s.EventCursor == initialCursor {
		t.Fatal(s)
	}
	var name string
	if err := f.store.DB().QueryRow(`SELECT name FROM projects WHERE id=?`, f.project).Scan(&name); err != nil || name != s.Presentation.Name {
		t.Fatal("name mismatch", err)
	}
	expect(t, f.replace("second", 1, "Second"), 200)
	expect(t, f.replace("stale", 1, "Third"), 409)
	before = f.counts()
	retry := f.replace("first", 0, "  Renamed <literal>  ")
	expect(t, retry, 200)
	if retry.Body.String() != firstReceipt || !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("replay changed historical receipt or state")
	}
	expect(t, f.replace("first", 0, "changed"), 409)
	expect(t, f.replace("noop-two", 2, "Second"), 200)
	if f.read().Presentation.Revision != 2 {
		t.Fatal("noop bumped revision")
	}
	w = f.call("GET", f.base()+"/history?limit=1", f.reader, "", nil)
	expect(t, w, 200)
	var page presentation.Page
	decode(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	next := f.call("GET", f.base()+"/history?limit=1&cursor="+url.QueryEscape(page.Cursor), f.reader, "", nil)
	expect(t, next, 200)
	decode(t, next, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 1 || page.HasMore {
		t.Fatal(page)
	}
	w = f.call("GET", f.base()+"/history/1", f.reader, "", nil)
	expect(t, w, 200)
	var old presentation.Revision
	decode(t, w, &old)
	if old.State.Name != "Renamed <literal>" || old.ActorID == "" || old.CreatedAt != old.State.UpdatedAt {
		t.Fatal(old)
	}
	expect(t, f.call("GET", f.base()+"/history/0", f.reader, "", nil), 400)
	expect(t, f.call("GET", f.base()+"/history/3", f.reader, "", nil), 404)
	if e := f.validate(); e != nil {
		t.Fatal(e)
	}
	if e := f.store.Close(); e != nil {
		t.Fatal(e)
	}
	var err error
	f.store, err = core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.mount()
	if f.read().Presentation.Revision != 2 {
		t.Fatal("restart lost state")
	}
	retry = f.replace("first", 0, "  Renamed <literal>  ")
	expect(t, retry, 200)
	if retry.Body.String() != firstReceipt {
		t.Fatal("restart replay changed")
	}
	if e := f.validate(); e != nil {
		t.Fatal(e)
	}
}
func TestPresentationAuthStrictRequestAndAtomicFailure(t *testing.T) {
	f := setup(t)
	request := map[string]any{"expected_revision": 0, "presentation": map[string]string{"name": "Renamed"}}
	expect(t, f.call("PUT", f.base(), f.token, "writer", request), 403)
	expect(t, f.call("PUT", f.base(), f.reader, "reader", request), 403)
	worker := f.worker
	f.worker = ""
	expect(t, f.call("PUT", f.base(), f.admin, "no-worker", request), 400)
	f.worker = worker
	before := f.counts()
	for i, raw := range []string{`{}`, `{"expected_revision":null,"presentation":{"name":"a"}}`, `{"expected_revision":-1,"presentation":{"name":"a"}}`, `{"expected_revision":9223372036854775807,"presentation":{"name":"a"}}`, `{"expected_revision":0,"presentation":null}`, `{"expected_revision":0,"presentation":{"name":"a","theme":null}}`, `{"expected_revision":0,"presentation":{"name":"a","brand_name":""}}`, `{"expected_revision":0,"presentation":{"name":"a","theme":{"canvas_text":"#e8e7e4"}}}`, `{"expected_revision":0,"presentation":{"name":"a","headings":{"jobs":"\ud800"}}}`, `{"expected_revision":0,"presentation":{"name":"a"},"other":1}`} {
		expect(t, f.raw("PUT", f.base(), f.admin, fmt.Sprint("bad", i), []byte(raw)), 400)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("invalid input wrote")
	}
	if _, e := f.store.DB().Exec(`CREATE TRIGGER fail_presentation_event BEFORE INSERT ON events WHEN NEW.event_type='project.presentation_changed' BEGIN SELECT RAISE(ABORT,'fixture'); END`); e != nil {
		t.Fatal(e)
	}
	expect(t, f.replace("fault", 0, "Changed"), 500)
	if !reflect.DeepEqual(before, f.counts()) || f.read().Presentation.Name != "one" {
		t.Fatal("transaction failed atomicity")
	}
	if _, e := f.store.DB().Exec(`DROP TRIGGER fail_presentation_event`); e != nil {
		t.Fatal(e)
	}
	expect(t, f.replace("fault", 0, "Changed"), 200)
	// Current authorization is authoritative even for an exact accepted key.
	hash := sha256.Sum256([]byte(f.admin))
	if _, e := f.store.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE token_sha256=?`, hex.EncodeToString(hash[:])); e != nil {
		t.Fatal(e)
	}
	before = f.counts()
	expect(t, f.replace("fault", 0, "Changed"), 403)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("unauthorized replay wrote")
	}
}
func TestPresentationCASAndCursorIsolation(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"A", "B"} {
		wg.Add(1)
		go func(name string) { defer wg.Done(); codes <- f.replace(name, 0, name).Code }(name)
	}
	wg.Wait()
	close(codes)
	success, conflicts := 0, 0
	for c := range codes {
		if c == 200 {
			success++
		} else if c == 409 {
			conflicts++
		} else {
			t.Fatal(c)
		}
	}
	if success != 1 || conflicts != 1 || f.read().Presentation.Revision != 1 {
		t.Fatal("CAS", success, conflicts)
	}
	expect(t, f.replace("next", 1, "Next"), 200)
	w := f.call("GET", f.base()+"/history?limit=1", f.admin, "", nil)
	expect(t, w, 200)
	var page presentation.Page
	decode(t, w, &page)
	expect(t, f.call("GET", f.base()+"/history?limit=1&cursor="+url.QueryEscape(page.Cursor), f.reader, "", nil), 400)
	worker := f.worker
	f.worker = ""
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/presentation/history?cursor="+url.QueryEscape(page.Cursor), f.admin, "", nil), 400)
	f.worker = worker
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/presentation", f.reader, "", nil), 403)
	for _, suffix := range []string{"?x=1", "/history?limit=0", "/history?limit=1&limit=2"} {
		expect(t, f.call("GET", f.base()+suffix, f.reader, "", nil), 400)
	}
}
func TestPresentationSnapshotCorruptionAndClockIndependentHistory(t *testing.T) {
	for _, kind := range []string{"name", "missing-history", "current-json", "history-json", "history-actor", "history-time", "history-gap", "orphan"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			expect(t, f.replace("first", 0, "First"), 200)
			expect(t, f.replace("second", 1, "Second"), 200)
			var q string
			switch kind {
			case "name":
				q = `UPDATE projects SET name='Mismatch' WHERE id=?`
			case "missing-history":
				q = `DELETE FROM project_presentation_history WHERE project_id=? AND revision=1`
			case "current-json":
				q = `UPDATE project_presentations SET state_json='{}' WHERE project_id=?`
			case "history-json":
				q = `UPDATE project_presentation_history SET state_json='{}' WHERE project_id=? AND revision=1`
			case "history-actor":
				q = `UPDATE project_presentation_history SET actor_id='' WHERE project_id=? AND revision=1`
			case "history-time":
				q = `UPDATE project_presentation_history SET created_at='bad' WHERE project_id=? AND revision=1`
			case "history-gap":
				q = `UPDATE project_presentation_history SET revision=4 WHERE project_id=? AND revision=1`
			case "orphan":
				conn, e := f.store.DB().Conn(t.Context())
				if e != nil {
					t.Fatal(e)
				}
				if _, e = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); e != nil {
					t.Fatal(e)
				}
				if _, e = conn.ExecContext(t.Context(), `UPDATE project_presentation_history SET project_id='missing_project' WHERE project_id=? AND revision=1`, f.project); e != nil {
					t.Fatal(e)
				}
				if _, e = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); e != nil {
					t.Fatal(e)
				}
				conn.Close()
				if e = f.validate(); e == nil {
					t.Fatal("orphan accepted")
				}
				return
			}
			if _, e := f.store.DB().Exec(q, f.project); e != nil {
				t.Fatal(e)
			}
			if e := f.validate(); e == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
	f := setup(t)
	expect(t, f.replace("first", 0, "First"), 200)
	// Retained timestamps are structural facts; revision, not wall time, orders history.
	var raw string
	if e := f.store.DB().QueryRow(`SELECT state_json FROM project_presentations WHERE project_id=?`, f.project).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var s presentation.State
	if e := json.Unmarshal([]byte(raw), &s); e != nil {
		t.Fatal(e)
	}
	s.UpdatedAt = "2099-01-01T00:00:00.000000000Z"
	b, _ := json.Marshal(s)
	if _, e := f.store.DB().Exec(`UPDATE project_presentations SET state_json=? WHERE project_id=?`, string(b), f.project); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DB().Exec(`UPDATE project_presentation_history SET state_json=?,created_at=? WHERE project_id=?`, string(b), s.UpdatedAt, f.project); e != nil {
		t.Fatal(e)
	}
	if e := f.validate(); e != nil {
		t.Fatal(e)
	}
	expect(t, f.replace("second", 1, "Second"), 200)
	if e := f.validate(); e != nil {
		t.Fatal("clock order was imposed", e)
	}
}
func TestPresentationFullReplacementAndQuietBoundedReads(t *testing.T) {
	f := setup(t)
	config := map[string]any{"name": " one ", "brand_name": " Literal <brand> ", "tagline": " Team workspace ", "headings": map[string]string{"jobs": " Work <script> ", "hero_description": "Text & context"}, "theme": map[string]string{"canvas_text": "#00000A"}}
	w := f.call("PUT", f.base(), f.admin, "configured", map[string]any{"expected_revision": 0, "presentation": config})
	expect(t, w, 200)
	s := f.read()
	if s.Presentation.BrandName != "Literal <brand>" || s.Presentation.Headings["jobs"] != "Work <script>" || s.Presentation.Theme["canvas_text"] != "#00000a" {
		t.Fatal(s)
	}
	if len(w.Body.Bytes()) > 512 || strings.Contains(w.Body.String(), "brand") {
		t.Fatal("receipt echoed config")
	}
	before := f.counts()
	for range 3 {
		f.read()
		expect(t, f.call("GET", f.base()+"/history", f.reader, "", nil), 200)
		expect(t, f.call("GET", f.base()+"/history/1", f.reader, "", nil), 200)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("reads wrote")
	}
	// Omitted overrides clear on full replacement; the retained revision is frozen.
	expect(t, f.replace("reset", 1, "one"), 200)
	s = f.read()
	if len(s.Presentation.Headings) != 0 || len(s.Presentation.Theme) != 0 || s.Presentation.BrandName != "FireController" || s.Presentation.Tagline != "Project workspace" {
		t.Fatal(s)
	}
	w = f.call("GET", f.base()+"/history/1", f.reader, "", nil)
	expect(t, w, 200)
	if !strings.Contains(w.Body.String(), `Literal \u003cbrand\u003e`) {
		t.Fatal("historical literal not retained", w.Body.String())
	}
	var payload string
	if e := f.store.DB().QueryRow(`SELECT payload_json FROM events WHERE project_id=? AND event_type='project.presentation_changed' ORDER BY sequence DESC LIMIT 1`, f.project).Scan(&payload); e != nil {
		t.Fatal(e)
	}
	if payload != `{"project_id":"`+f.project+`"}` {
		t.Fatal("noncompact event", payload)
	}
	// Context cancellation must not produce a receipt, event, history or rename.
	before = f.counts()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest("PUT", f.base(), strings.NewReader(`{"expected_revision":2,"presentation":{"name":"cancelled"}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", f.worker)
	r.Header.Set("Idempotency-Key", "cancelled")
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code == 200 || !reflect.DeepEqual(before, f.counts()) || f.read().Presentation.Name != "one" {
		t.Fatal("cancelled write took effect")
	}
}
