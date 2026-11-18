package tracking_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/benhook1013/FireController/internal/tracking"
)

type fixture struct {
	t                                     *testing.T
	store                                 *core.Store
	mux                                   *http.ServeMux
	admin, project, other, writer, reader string
	worker                                string
	keys                                  int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
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
	if err = tracking.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	// Installing twice is a no-op.
	if err = tracking.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, admin: strings.TrimSpace(string(token))}
	mux := http.NewServeMux()
	mux.Handle("/", server.New(store, server.WithRepositoryResolver(func(_ context.Context, name string) (server.RepositoryIdentity, error) {
		id := int64(1)
		if strings.HasSuffix(name, "two") {
			id = 2
		}
		return server.RepositoryIdentity{ID: id, FullName: name}, nil
	})))
	jobs.Register(mux, store)
	tracking.Register(mux, store)
	f.mux = mux
	f.project = f.newProject("one")
	f.other = f.newProject("two")
	f.writer = f.grant("writer", "read", "write")
	f.reader = f.grant("reader", "read")
	var v struct {
		ID string `json:"id"`
	}
	f.decode(f.call("POST", "/api/v1/projects/"+f.project+"/workers", f.admin, map[string]string{"name": "worker"}), 200, &v)
	f.worker = v.ID
	return f
}

func (f *fixture) call(method, path, token string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(body); err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if f.worker != "" {
		r.Header.Set("X-FC-Worker", f.worker)
	}
	if method != "GET" {
		f.keys++
		r.Header.Set("Idempotency-Key", "key-"+strconv.Itoa(f.keys))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}
func (f *fixture) decode(w *httptest.ResponseRecorder, status int, v any) {
	f.t.Helper()
	if w.Code != status {
		f.t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
	}
	if v != nil {
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			f.t.Fatal(err)
		}
	}
}
func (f *fixture) code(w *httptest.ResponseRecorder, status int, code string) {
	f.t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	f.decode(w, status, &e)
	if e.Error.Code != code {
		f.t.Fatalf("code %q want %q: %s", e.Error.Code, code, w.Body.String())
	}
}
func (f *fixture) newProject(name string) string {
	var v struct {
		ID string `json:"id"`
	}
	f.decode(f.call("POST", "/api/v1/projects", f.admin, map[string]string{"name": name, "repository": "owner/" + name}), 200, &v)
	return v.ID
}
func (f *fixture) grant(token string, permissions ...string) string {
	sum := sha256.Sum256([]byte(token))
	f.decode(f.call("POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions}), 200, nil)
	return token
}
func (f *fixture) snapshot() {
	f.t.Helper()
	tx, err := f.store.DB().BeginTx(f.t.Context(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	if err = tracking.ValidateSnapshot(f.t.Context(), tx); err != nil {
		f.t.Fatalf("snapshot: %v", err)
	}
}
func (f *fixture) base() string { return "/api/v1/projects/" + f.project + "/tracking" }
func (f *fixture) receipt(w *httptest.ResponseRecorder, revision int64, outcome string) {
	f.t.Helper()
	var r tracking.Receipt
	f.decode(w, 200, &r)
	if r.Revision != revision || r.Outcome != outcome {
		f.t.Fatalf("receipt %+v want revision %d %s", r, revision, outcome)
	}
}
func (f *fixture) item(key string) tracking.ItemView {
	f.t.Helper()
	var v tracking.ItemView
	f.decode(f.call("GET", f.base()+"/items/"+key, f.reader, nil), 200, &v)
	return v
}
func (f *fixture) list(query string) []string {
	f.t.Helper()
	var page tracking.Page[tracking.ItemSummary]
	f.decode(f.call("GET", f.base()+"/items"+query, f.reader, nil), 200, &page)
	var keys []string
	for _, i := range page.Items {
		keys = append(keys, i.Key)
	}
	return keys
}

func TestMilestonesItemsAndCounts(t *testing.T) {
	f := setup(t)
	f.receipt(f.call("PUT", f.base()+"/milestones/v1", f.writer, map[string]any{"milestone": map[string]any{"title": " First release ", "target_date": "2027-01-31"}}), 1, "created")
	f.code(f.call("PUT", f.base()+"/milestones/v1", f.writer, map[string]any{"expected_revision": 0, "milestone": map[string]any{"title": "Other"}}), 409, "revision_conflict")
	f.code(f.call("PUT", f.base()+"/milestones/v2", f.writer, map[string]any{"milestone": map[string]any{"title": "x", "target_date": "2027-02-30"}}), 400, "validation_failed")

	f.receipt(f.call("PUT", f.base()+"/items/GR", f.writer, map[string]any{"item": map[string]any{"title": "Grid", "labels": []string{"area:world"}}}), 1, "created")
	f.receipt(f.call("PUT", f.base()+"/items/GR-2.1", f.writer, map[string]any{"item": map[string]any{"title": "Room grid", "milestone": "v1", "parent": "GR", "labels": []string{"area:world", "area:world", "core"}}}), 1, "created")
	f.receipt(f.call("PUT", f.base()+"/items/GR-2.2", f.writer, map[string]any{"item": map[string]any{"title": "Exits", "milestone": "v1", "parent": "GR", "status": "active"}}), 1, "created")
	f.code(f.call("PUT", f.base()+"/items/GR-9", f.writer, map[string]any{"item": map[string]any{"title": "Bad", "milestone": "v9"}}), 400, "validation_failed")
	f.code(f.call("PUT", f.base()+"/items/GR-9", f.writer, map[string]any{"item": map[string]any{"title": "Bad", "status": "finished"}}), 400, "validation_failed")

	got := f.item("GR-2.1")
	if got.Item.Status != "planned" || got.Item.Title != "Room grid" || strings.Join(got.Item.Labels, ",") != "area:world,core" || got.Item.Parent != "GR" || got.Item.Milestone != "v1" || got.Item.Links == nil {
		t.Fatalf("defaults/normalization: %+v", got)
	}
	parent := f.item("GR")
	if parent.Children.Planned != 1 || parent.Children.Active != 1 || parent.Children.Total != 2 {
		t.Fatalf("child counts: %+v", parent.Children)
	}

	// Cheap status change without a guard, then an unchanged repeat.
	f.receipt(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, map[string]any{"changes": map[string]any{"status": "done"}}), 2, "updated")
	f.receipt(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, map[string]any{"changes": map[string]any{"status": "done"}}), 2, "unchanged")
	// Guarded change with a stale revision.
	f.code(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, map[string]any{"expected_revision": 1, "changes": map[string]any{"status": "active"}}), 409, "revision_conflict")
	// Title needs the guard.
	f.code(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, map[string]any{"changes": map[string]any{"title": "New"}}), 400, "validation_failed")
	f.receipt(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, map[string]any{"expected_revision": 2, "changes": map[string]any{"title": "New", "add_labels": []string{"v1-gate"}, "remove_labels": []string{"core"}}}), 3, "updated")
	if got = f.item("GR-2.1"); got.Item.Title != "New" || strings.Join(got.Item.Labels, ",") != "area:world,v1-gate" {
		t.Fatalf("patched: %+v", got)
	}
	// Explicit null is refused rather than read as omission.
	f.code(f.call("PATCH", f.base()+"/items/GR-2.1", f.writer, `{"changes":{"status":null}}`), 400, "validation_failed")

	var m tracking.MilestoneView
	f.decode(f.call("GET", f.base()+"/milestones/v1", f.reader, nil), 200, &m)
	if m.Milestone.Title != "First release" || m.Counts.Done != 1 || m.Counts.Active != 1 || m.Counts.Total != 2 || len(m.OpenItems) != 1 || m.OpenItems[0].Key != "GR-2.2" {
		t.Fatalf("milestone view: %+v", m)
	}
	var ms tracking.Page[tracking.MilestoneSummary]
	f.decode(f.call("GET", f.base()+"/milestones?state=open", f.reader, nil), 200, &ms)
	if len(ms.Items) != 1 || ms.Items[0].Counts.Total != 2 {
		t.Fatalf("milestone list: %+v", ms)
	}
	f.receipt(f.call("PATCH", f.base()+"/milestones/v1", f.writer, map[string]any{"changes": map[string]any{"state": "closed"}}), 2, "updated")
	f.decode(f.call("GET", f.base()+"/milestones?state=open", f.reader, nil), 200, &ms)
	if len(ms.Items) != 0 {
		t.Fatalf("closed milestone still listed open: %+v", ms)
	}

	if keys := strings.Join(f.list("?milestone=v1&status=planned,active"), ","); keys != "GR-2.2" {
		t.Fatalf("filtered list: %s", keys)
	}
	if keys := strings.Join(f.list("?label=area:world"), ","); keys != "GR,GR-2.1" {
		t.Fatalf("label list: %s", keys)
	}
	if keys := strings.Join(f.list("?parent=GR"), ","); keys != "GR-2.1,GR-2.2" {
		t.Fatalf("parent list: %s", keys)
	}

	// Parent cycles are refused.
	f.code(f.call("PATCH", f.base()+"/items/GR", f.writer, map[string]any{"changes": map[string]any{"parent": "GR-2.1"}}), 400, "validation_failed")

	// Readers cannot write and other projects cannot see the records.
	f.code(f.call("PATCH", f.base()+"/items/GR", f.reader, map[string]any{"changes": map[string]any{"status": "done"}}), 403, "forbidden")
	w := f.call("GET", "/api/v1/projects/"+f.other+"/tracking/items/GR", f.writer, nil)
	if w.Code != 403 && w.Code != 404 {
		t.Fatalf("cross-project read: %d %s", w.Code, w.Body.String())
	}
}

func TestLinksAreTypedIdempotentSets(t *testing.T) {
	f := setup(t)
	f.receipt(f.call("PUT", f.base()+"/items/A", f.writer, map[string]any{"item": map[string]any{"title": "A"}}), 1, "created")
	f.receipt(f.call("PUT", f.base()+"/items/B", f.writer, map[string]any{"item": map[string]any{"title": "B"}}), 1, "created")
	var job struct {
		ID string `json:"id"`
	}
	f.decode(f.call("POST", "/api/v1/projects/"+f.project+"/jobs", f.writer, map[string]any{"title": "Job"}), 201, &job)

	links := map[string]any{"add": []map[string]string{
		{"type": "pr", "ref": "42", "role": "delivers"},
		{"type": "pr", "ref": "other/repo#7", "role": "related", "label": "upstream"},
		{"type": "job", "ref": job.ID, "role": "delivers"},
		{"type": "item", "ref": "B", "role": "blocks"},
		{"type": "url", "ref": "https://ci.example/run/1", "role": "proof"},
	}}
	f.receipt(f.call("POST", f.base()+"/items/A/links", f.writer, links), 2, "updated")
	f.receipt(f.call("POST", f.base()+"/items/A/links", f.writer, links), 2, "unchanged")
	got := f.item("A").Item
	if len(got.Links) != 5 || got.Links[3].Ref != "owner/one#42" || got.Links[2].Ref != "other/repo#7" || got.Links[2].Label != "upstream" {
		t.Fatalf("links: %+v", got.Links)
	}
	if keys := strings.Join(f.list("?link=pr:%2342"), ","); keys != "A" {
		t.Fatalf("pr reverse lookup: %s", keys)
	}
	if keys := strings.Join(f.list("?link=job:"+job.ID), ","); keys != "A" {
		t.Fatalf("job reverse lookup: %s", keys)
	}

	f.code(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"add": []map[string]string{{"type": "job", "ref": "job_missing", "role": "delivers"}}}), 400, "validation_failed")
	f.code(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"add": []map[string]string{{"type": "item", "ref": "A", "role": "related"}}}), 400, "validation_failed")
	f.code(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"add": []map[string]string{{"type": "url", "ref": "javascript:alert(1)", "role": "proof"}}}), 400, "validation_failed")
	f.code(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"add": []map[string]string{{"type": "pr", "ref": "42", "role": "owns"}}}), 400, "validation_failed")

	// Removing without a role removes every role for that target; missing targets are no-ops.
	f.receipt(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"remove": []map[string]string{{"type": "pr", "ref": "#42"}, {"type": "pr", "ref": "999"}}}), 3, "updated")
	if keys := f.list("?link=pr:42"); len(keys) != 0 {
		t.Fatalf("removed link still indexed: %v", keys)
	}
	f.code(f.call("POST", f.base()+"/items/A/links", f.writer, map[string]any{"expected_revision": 2, "remove": []map[string]string{{"type": "item", "ref": "B"}}}), 409, "revision_conflict")

	// A full declaration without links keeps them.
	f.receipt(f.call("PUT", f.base()+"/items/A", f.writer, map[string]any{"item": map[string]any{"title": "A", "status": "active"}}), 4, "updated")
	if got = f.item("A").Item; len(got.Links) != 4 {
		t.Fatalf("declaration dropped links: %+v", got.Links)
	}

	var history tracking.Page[tracking.HistorySummary]
	f.decode(f.call("GET", f.base()+"/items/A/history?limit=2", f.reader, nil), 200, &history)
	if len(history.Items) != 2 || history.Items[0].Revision != 4 || !history.HasMore {
		t.Fatalf("history: %+v", history)
	}
	var rev tracking.ItemRevision
	f.decode(f.call("GET", f.base()+"/items/A/history/1", f.reader, nil), 200, &rev)
	if rev.State.Revision != 1 || len(rev.State.Links) != 0 {
		t.Fatalf("revision 1: %+v", rev)
	}
	f.snapshot()
}

func TestBatchIsAtomicAndReapplyable(t *testing.T) {
	f := setup(t)
	doc := map[string]any{
		"milestones": []map[string]any{{"key": "v1", "milestone": map[string]any{"title": "v1"}}},
		"items": []map[string]any{
			{"key": "P", "item": map[string]any{"title": "Parent", "milestone": "v1"}},
			{"key": "P.1", "item": map[string]any{"title": "Child", "parent": "P", "milestone": "v1", "links": []map[string]string{{"type": "url", "ref": "https://example.test/doc", "role": "related"}}}},
		},
	}
	var out tracking.BatchReceipt
	f.decode(f.call("POST", f.base()+"/batch", f.writer, doc), 200, &out)
	if len(out.Results) != 3 || out.Results[2].Outcome != "created" {
		t.Fatalf("batch: %+v", out)
	}
	f.decode(f.call("POST", f.base()+"/batch", f.writer, doc), 200, &out)
	for _, r := range out.Results {
		if r.Outcome != "unchanged" {
			t.Fatalf("reapply changed %+v", r)
		}
	}
	bad := map[string]any{"items": []map[string]any{
		{"key": "Q", "item": map[string]any{"title": "New"}},
		{"key": "R", "item": map[string]any{"title": "Broken", "parent": "missing"}},
	}}
	w := f.call("POST", f.base()+"/batch", f.writer, bad)
	f.code(w, 400, "validation_failed")
	if !strings.Contains(w.Body.String(), "item R") {
		t.Fatalf("batch error does not name the record: %s", w.Body.String())
	}
	f.code(f.call("GET", f.base()+"/items/Q", f.reader, nil), 404, "not_found")
}

func TestReviewRegressions(t *testing.T) {
	f := setup(t)
	put := func(key, parent string) {
		t.Helper()
		item := map[string]any{"title": key}
		if parent != "" {
			item["parent"] = parent
		}
		f.receipt(f.call("PUT", f.base()+"/items/"+key, f.writer, map[string]any{"item": item}), 1, "created")
	}
	// Two 20-deep chains: moving one under the other would exceed 32 levels.
	for i := 0; i < 20; i++ {
		pa, pb := "", ""
		if i > 0 {
			pa, pb = "A"+strconv.Itoa(i-1), "B"+strconv.Itoa(i-1)
		}
		put("A"+strconv.Itoa(i), pa)
		put("B"+strconv.Itoa(i), pb)
	}
	f.code(f.call("PATCH", f.base()+"/items/B0", f.writer, map[string]any{"changes": map[string]any{"parent": "A19"}}), 400, "validation_failed")
	// Descendants stay writable regardless of depth checks on unchanged parents.
	f.receipt(f.call("PATCH", f.base()+"/items/B19", f.writer, map[string]any{"changes": map[string]any{"status": "done"}}), 2, "updated")

	// Swapping 10 labels for 10 others in one change respects the final count only.
	var old, fresh []string
	for i := 0; i < 10; i++ {
		old, fresh = append(old, "old"+strconv.Itoa(i)), append(fresh, "new"+strconv.Itoa(i))
	}
	f.receipt(f.call("PATCH", f.base()+"/items/A0", f.writer, map[string]any{"changes": map[string]any{"labels": old}}), 2, "updated")
	f.receipt(f.call("PATCH", f.base()+"/items/A0", f.writer, map[string]any{"changes": map[string]any{"add_labels": fresh, "remove_labels": old}}), 3, "updated")

	// A create-only retry of the identical declaration converges.
	decl := map[string]any{"expected_revision": 0, "item": map[string]any{"title": "Once"}}
	f.receipt(f.call("PUT", f.base()+"/items/ONCE", f.writer, decl), 1, "created")
	f.receipt(f.call("PUT", f.base()+"/items/ONCE", f.writer, decl), 1, "unchanged")
	f.code(f.call("PUT", f.base()+"/items/ONCE", f.writer, map[string]any{"expected_revision": 0, "item": map[string]any{"title": "Different"}}), 409, "revision_conflict")
	f.snapshot()
}
