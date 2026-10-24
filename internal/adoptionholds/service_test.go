package adoptionholds_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

type fixture struct {
	t          *testing.T
	store      *core.Store
	service    *h.Service
	mux        *http.ServeMux
	dir, token string
}

const base = "/api/v1/projects/project_one/reviews/adoption-obligations"

func fresh(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "private")}
	f.open()
	body, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.token = strings.TrimSpace(string(body))
	f.exec(`INSERT INTO projects VALUES('project_one','One',17,'owner/one'),('project_two','Two',18,'owner/two')`)
	f.exec(`INSERT INTO workers VALUES('project_one','worker_one','One'),('project_two','worker_two','Two')`)
	t.Cleanup(func() { f.store.Close() })
	return f
}
func (f *fixture) open() {
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = h.EnsureSchema(context.Background(), f.store); err != nil {
		f.t.Fatal(err)
	}
	f.service = h.New(f.store)
	f.mux = http.NewServeMux()
	f.service.Register(f.mux)
}
func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB().Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) call(method, path, token, worker, key string, input any) *httptest.ResponseRecorder {
	f.t.Helper()
	var body []byte
	var err error
	if raw, ok := input.([]byte); ok {
		body = raw
	} else if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	out := httptest.NewRecorder()
	f.mux.ServeHTTP(out, r)
	return out
}
func decode[T any](t *testing.T, r *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status%d want%d: %s", r.Code, status, r.Body.String())
	}
	var out T
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func batch() m.Batch {
	zero := int64(0)
	return m.Batch{Version: 1, Source: m.Source{Namespace: "sealed-source", RepositoryID: 17, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []m.Obligation{{SourceKey: "finding-1", Kind: "finding", AffectedPullNumbers: []int64{3251}, Summary: "PRIVATE original <inert> finding", Evidence: []string{"source:run-1/finding-1"}, OriginalStatus: "pending"}, {SourceKey: "route-1", Kind: "route", AffectedPullNumbers: []int64{3105, 3120}, Summary: "PRIVATE original route", Evidence: []string{}, OriginalStatus: "open"}}}
}
func (f *fixture) preview(b m.Batch) m.Preview {
	return decode[m.Preview](f.t, f.call("POST", base+"/preview", f.token, "worker_one", "", b), 200)
}
func (f *fixture) apply(b m.Batch, key string) m.Receipt {
	p := f.preview(b)
	return decode[m.Receipt](f.t, f.call("POST", base+"/apply", f.token, "worker_one", key, m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}), 200)
}
func (f *fixture) page(query string) m.Page {
	return decode[m.Page](f.t, f.call("GET", base+query, f.token, "", "", nil), 200)
}
func (f *fixture) validate() {
	f.t.Helper()
	if err := h.ValidateSnapshot(context.Background(), f.store.DB()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(table string) int64 {
	f.t.Helper()
	var n int64
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) scope() core.Scope {
	r := httptest.NewRequest("GET", base, nil)
	r.SetPathValue("project_id", "project_one")
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Worker", "worker_one")
	s, err := f.store.AdminScope(r)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func TestAdditiveResolutionExactReplayAndConservation(t *testing.T) {
	f := fresh(t)
	b := batch()
	before := f.count("receipts")
	p := f.preview(b)
	if p.New != 2 || p.SourceRevision != 0 || f.count("receipts") != before || f.count("review_adoption_sources") != 0 {
		t.Fatal("preview mutated or miscounted")
	}
	request := m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	applied := f.call("POST", base+"/apply", f.token, "worker_one", "apply", request)
	receipt := decode[m.Receipt](t, applied, 200)
	if receipt.Revision != 1 || receipt.New != 2 {
		t.Fatal(receipt)
	}
	page := f.page("?status=all")
	if len(page.Items) != 2 || strings.Contains(f.call("GET", base, f.token, "", "", nil).Body.String(), "PRIVATE") {
		t.Fatal("list disclosed original ledger")
	}
	var id string
	for _, item := range page.Items {
		if item.SourceKey == "finding-1" {
			id = item.ID
		}
	}
	detail := decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 200)
	if detail.Original.Summary != b.Obligations[0].Summary || detail.Revision != 1 || detail.Source != b.Source {
		t.Fatal("original changed")
	}
	for _, n := range []int64{3251, 3105, 3120} {
		open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", n)
		if err != nil || !open {
			t.Fatal(n, err)
		}
	}
	for _, v := range []struct {
		p string
		n int64
	}{{"project_two", 3251}, {"project_one", 1}} {
		open, err := h.HasOpen(t.Context(), f.store.DB(), v.p, v.n)
		if err != nil || open {
			t.Fatal("unrelated held", err)
		}
	}
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: "Explicit later native resolution", Evidence: []string{"commit:verified"}}
	resolved := f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "resolve", resolution)
	decode[map[string]any](t, resolved, 200)
	if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err != nil || open {
		t.Fatal("resolution ineffective", err)
	}
	if got := f.call("POST", base+"/apply", f.token, "worker_one", "apply", request); got.Code != 200 || got.Body.String() != applied.Body.String() {
		t.Fatal("original replay changed", got.Body.String())
	}
	if got := f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "resolve", resolution); got.Code != 200 || got.Body.String() != resolved.Body.String() {
		t.Fatal("resolution replay changed")
	}
	decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "stale", resolution), 409)
	b.ExpectedRevision = &one
	if got := f.apply(b, "repeat"); got.Outcome != "unchanged" || got.Revision != 1 {
		t.Fatal(got)
	}
	b.Obligations = append(b.Obligations, m.Obligation{SourceKey: "finding-2", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "New original", Evidence: []string{}, OriginalStatus: "pending"})
	if got := f.apply(b, "additional"); got.Revision != 2 || got.New != 1 || got.Unchanged != 2 {
		t.Fatal(got)
	}
	detail = decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 200)
	if detail.Status != "resolved" || detail.Resolution == nil || detail.Resolution.Reason != resolution.Reason || detail.Original.Summary != batch().Obligations[0].Summary {
		t.Fatal("additive import changed resolution")
	}
	if err := h.VerifySource(t.Context(), f.store.DB(), "project_one", b.Source, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.VerifySource(t.Context(), f.store.DB(), "project_one", b.Source, 1); err == nil {
		t.Fatal("stale proof passed")
	}
	if err := h.VerifyRetainedSource(t.Context(), f.store.DB(), "project_one", b.Source, 1); err != nil {
		t.Fatal("later additive import invalidated retained policy provenance", err)
	}
	f.validate()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	f.validate()
	if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err != nil || open {
		t.Fatal("restart changed resolved state")
	}
}

func TestResultHoldRemainsIndependentOfFindingResolutionWithoutNativeCredit(t *testing.T) {
	f := fresh(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	nativeTables := []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_execution_reservations", "review_round_controls", "review_round_completions"}
	for _, table := range nativeTables {
		if f.count(table) != 0 {
			t.Fatal("native fixture was not empty", table)
		}
	}
	b := batch()
	b.Obligations = []m.Obligation{b.Obligations[0], {SourceKey: "result-1", Kind: "result", AffectedPullNumbers: []int64{3251}, Summary: "PRIVATE terminal result accounting remains unsettled", Evidence: []string{"source:terminal-result-1"}, OriginalStatus: "pending"}}
	p := f.preview(b)
	request := m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	applied := f.call("POST", base+"/apply", f.token, "worker_one", "result-import", request)
	if receipt := decode[m.Receipt](t, applied, 200); receipt.New != 2 || receipt.Revision != 1 {
		t.Fatal("mixed finding/result import did not retain both originals", receipt)
	}
	items := f.page("?pull_number=3251").Items
	ids := map[string]string{}
	for _, item := range items {
		ids[item.Kind] = item.ID
	}
	if len(items) != 2 || ids["finding"] == "" || ids["result"] == "" || ids["finding"] == ids["result"] {
		t.Fatal("result and finding did not retain separate identities", items)
	}
	detail := decode[m.Detail](t, f.call("GET", base+"/"+ids["result"], f.token, "", "", nil), 200)
	if !reflect.DeepEqual(detail.Original, b.Obligations[1]) || detail.Source != b.Source || detail.Status != "open" || detail.Resolution != nil {
		t.Fatal("result original or source binding changed", detail)
	}
	if facts, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251); err != nil || facts.Open != 2 {
		t.Fatal("result did not participate in shared hold facts", facts, err)
	}
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: "Finding correction verified", Evidence: []string{"commit:finding-fixed"}}
	decode[map[string]any](t, f.call("POST", base+"/"+ids["finding"]+"/resolve", f.token, "worker_one", "finding-resolved", resolution), 200)
	if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err != nil || !open {
		t.Fatal("finding resolution incorrectly released unsettled result", err)
	}
	if items := f.page("?pull_number=3251").Items; len(items) != 1 || items[0].Kind != "result" || items[0].Status != "open" || items[0].Revision != 1 {
		t.Fatal("finding resolution changed independent result hold", items)
	}
	if facts, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251); err != nil || facts.Open != 1 {
		t.Fatal("result-only shared hold facts changed", facts, err)
	}
	if replay := f.call("POST", base+"/apply", f.token, "worker_one", "result-import", request); replay.Code != 200 || replay.Body.String() != applied.Body.String() {
		t.Fatal("finding resolution invalidated original mixed-batch replay", replay.Body.String())
	}
	f.validate()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	f.validate()
	detail = decode[m.Detail](t, f.call("GET", base+"/"+ids["result"], f.token, "", "", nil), 200)
	if detail.Status != "open" || !reflect.DeepEqual(detail.Original, b.Obligations[1]) {
		t.Fatal("restart or snapshot normalization lost result hold", detail)
	}
	resolution.Reason, resolution.Evidence = "Destination accounting explicitly verified", []string{"native:accounting-proof"}
	readerToken := "result-reader"
	sum := sha256.Sum256([]byte(readerToken))
	f.exec(`INSERT INTO credentials VALUES('cred_result_reader','principal_result_reader',?,0,0)`, hex.EncodeToString(sum[:]))
	f.exec(`INSERT INTO grants VALUES('cred_result_reader','project_one',1,0)`)
	decode[map[string]any](t, f.call("POST", base+"/"+ids["result"]+"/resolve", readerToken, "worker_one", "not-admin", resolution), 403)
	resolved := f.call("POST", base+"/"+ids["result"]+"/resolve", f.token, "worker_one", "result-resolved", resolution)
	decode[map[string]any](t, resolved, 200)
	if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err != nil || open {
		t.Fatal("explicit result resolution did not release hold", err)
	}
	if replay := f.call("POST", base+"/apply", f.token, "worker_one", "result-import", request); replay.Code != 200 || replay.Body.String() != applied.Body.String() {
		t.Fatal("result resolution invalidated original batch replay", replay.Body.String())
	}
	detail = decode[m.Detail](t, f.call("GET", base+"/"+ids["result"], f.token, "", "", nil), 200)
	if detail.Status != "resolved" || detail.Resolution == nil || detail.Resolution.Reason != resolution.Reason || !reflect.DeepEqual(detail.Original, b.Obligations[1]) {
		t.Fatal("result original or explicit resolution was lost", detail)
	}
	for _, table := range nativeTables {
		if f.count(table) != 0 {
			t.Fatal("result import/resolution/replay fabricated native review or count effects", table)
		}
	}
	f.validate()
}

func TestSnapshotRefusesResealedUnknownResultKind(t *testing.T) {
	f := fresh(t)
	b := batch()
	b.Obligations = b.Obligations[:1]
	b.Obligations[0].Kind = "result"
	f.apply(b, "result")
	f.validate()
	b.Obligations[0].Kind = "unknown-result"
	body := m.Encode(b)
	original := m.Encode(b.Obligations[0])
	f.exec(`DROP TRIGGER review_adoption_originals_immutable`)
	f.exec(`DROP TRIGGER review_adoption_imports_immutable`)
	f.exec(`UPDATE review_adoption_obligations SET original_json=?,original_sha256=?`, string(original), m.Digest(original))
	f.exec(`UPDATE review_adoption_imports SET batch_json=?,batch_sha256=?`, string(body), m.Digest(body))
	for _, statement := range h.SchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_originals_immutable ") || strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_imports_immutable ") {
			f.exec(statement)
		}
	}
	if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("resealed unknown kind passed canonical snapshot normalization")
	}
	if _, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err == nil {
		t.Fatal("unknown stored kind silently released result hold")
	}
}
func TestStrictBoundsScopePaginationAndAuth(t *testing.T) {
	f := fresh(t)
	b := batch()
	f.apply(b, "seed")
	for _, query := range []string{"?pull_number=0", "?pull_number=+1", "?pull_number=1&pull_number=2", "?status=bad", "?limit=0", "?limit=101", "?limit=1&cursor=bad", "?unexpected=x"} {
		decode[map[string]any](t, f.call("GET", base+query, f.token, "", "", nil), 400)
	}
	page := f.page("?status=all&limit=1")
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal("pagination")
	}
	next := f.page("?status=all&limit=1&cursor=" + page.NextCursor)
	if len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID || next.NextCursor != "" {
		t.Fatal("pagination repeated")
	}
	decode[map[string]any](t, f.call("GET", base+"?status=open&limit=1&cursor="+page.NextCursor, f.token, "", "", nil), 400)
	decode[map[string]any](t, f.call("POST", base+"/preview", f.token, "", "", b), 403)
	decode[map[string]any](t, f.call("GET", base, "", "", "", nil), 401)
	token := "readonly"
	sum := sha256.Sum256([]byte(token))
	f.exec(`INSERT INTO credentials VALUES('cred_reader','principal_reader',?,0,0)`, hex.EncodeToString(sum[:]))
	f.exec(`INSERT INTO grants VALUES('cred_reader','project_one',1,0)`)
	decode[m.Page](t, f.call("GET", base, token, "", "", nil), 200)
	decode[map[string]any](t, f.call("POST", base+"/preview", token, "worker_one", "", b), 403)
	p := f.preview(func() m.Batch { v := batch(); one := int64(1); v.ExpectedRevision = &one; return v }())
	if p.Unchanged != 2 {
		t.Fatal(p)
	}
	for _, raw := range []string{`{"version":1,"version":1}`, `{"version":1,"unknown":"PRIVATE"}`, `null`, `{"expected_revision":null}`} {
		r := f.call("POST", base+"/preview", f.token, "worker_one", "", []byte(raw))
		decode[map[string]any](t, r, 400)
		if strings.Contains(r.Body.String(), "PRIVATE") {
			t.Fatal("private error")
		}
	}
	changed := batch()
	one := int64(1)
	changed.ExpectedRevision = &one
	changed.Obligations[0].Summary = "changed"
	decode[map[string]any](t, f.call("POST", base+"/preview", f.token, "worker_one", "", changed), 409)
	changed = batch()
	changed.Source.SnapshotSHA256 = strings.Repeat("b", 64)
	changed.ExpectedRevision = &one
	decode[map[string]any](t, f.call("POST", base+"/preview", f.token, "worker_one", "", changed), 409)
	b.Source.RepositoryID = 18
	decode[map[string]any](t, f.call("POST", base+"/preview", f.token, "worker_one", "", b), 409)
	for name, change := range map[string]func(*m.Batch){"empty": func(v *m.Batch) { v.Obligations = nil }, "overcount": func(v *m.Batch) { v.Obligations = make([]m.Obligation, 101) }, "summary": func(v *m.Batch) { v.Obligations[0].Summary = strings.Repeat("x", 16385) }, "duplicate": func(v *m.Batch) { v.Obligations[1] = v.Obligations[0] }, "duplicate-pr": func(v *m.Batch) { v.Obligations[0].AffectedPullNumbers = []int64{1, 1} }, "pr": func(v *m.Batch) { v.Obligations[0].AffectedPullNumbers = []int64{2147483648} }, "nil-evidence": func(v *m.Batch) { v.Obligations[0].Evidence = nil }, "evidence": func(v *m.Batch) { v.Obligations[0].Evidence = []string{strings.Repeat("x", 1025)} }, "kind": func(v *m.Batch) { v.Obligations[0].Kind = "instruction" }, "status": func(v *m.Batch) { v.Obligations[0].OriginalStatus = "resolved" }} {
		t.Run(name, func(t *testing.T) {
			v := batch()
			change(&v)
			if _, _, err := h.NormalizeBatch(v); err == nil {
				t.Fatal("invalid admitted")
			}
		})
	}
	large := batch()
	large.Obligations = nil
	for i := 0; i < 20; i++ {
		o := batch().Obligations[0]
		o.SourceKey = fmt.Sprint(i)
		o.Summary = strings.Repeat("x", 16384)
		large.Obligations = append(large.Obligations, o)
	}
	if _, _, err := h.NormalizeBatch(large); err == nil {
		t.Fatal("wrapper budget bypass")
	}
	f.validate()
}
func TestProtectiveHoldPreservesPendingWorkAndRollbackReplayAuthorization(t *testing.T) {
	f := fresh(t)
	b := batch()
	p := f.preview(b)
	f.exec(`CREATE TABLE review_operations(project_id TEXT,pull_number INTEGER,state TEXT)`)
	f.exec(`INSERT INTO review_operations VALUES('project_one',3251,'active')`)
	request := m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	decode[m.Receipt](t, f.call("POST", base+"/apply", f.token, "worker_one", "protective", request), 200)
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_operations`).Scan(&state); err != nil || state != "active" {
		t.Fatal("protective import changed native work", err)
	}
	if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err != nil || !open {
		t.Fatal("pending native work prevented protective hold", err)
	}
	other := batch()
	other.Source.Namespace = "another-source"
	p = f.preview(other)
	before := f.count("receipts")
	f.exec(`CREATE TRIGGER injected_abort BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'owned failure'); END`)
	decode[map[string]any](t, f.call("POST", base+"/apply", f.token, "worker_one", "rollback", m.ApplyRequest{Batch: other, PreviewSHA256: p.PreviewSHA256}), 500)
	if f.count("review_adoption_obligations") != 2 || f.count("review_adoption_imports") != 1 || f.count("review_adoption_sources") != 1 || f.count("receipts") != before {
		t.Fatal("failed event partial effects")
	}
	f.exec(`DROP TRIGGER injected_abort`)
	f.exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`)
	decode[map[string]any](t, f.call("POST", base+"/apply", f.token, "worker_one", "protective", request), 401)
}
func TestEscapedOversizeRequestsRefuseWithoutChangingRetainedSnapshot(t *testing.T) {
	for name, escaped := range map[string]string{"html": "<", "quote": `"`, "control": "\n"} {
		t.Run(name, func(t *testing.T) {
			f := fresh(t)
			f.apply(batch(), "seed")
			id := f.page("?status=all").Items[0].ID
			tables := []string{"review_adoption_sources", "review_adoption_imports", "review_adoption_obligations", "review_adoption_targets", "review_adoption_resolutions", "receipts", "events"}
			before := make(map[string]int64, len(tables))
			for _, table := range tables {
				before[table] = f.count(table)
			}
			one := int64(1)
			b := batch()
			b.ExpectedRevision = &one
			b.Obligations = []m.Obligation{{SourceKey: "oversize", Kind: "finding", AffectedPullNumbers: []int64{1}, Summary: strings.Repeat(escaped, (16<<10)-1) + "x", Evidence: []string{}, OriginalStatus: "pending"}}
			decode[map[string]any](t, f.call("POST", base+"/preview", f.token, "worker_one", "", b), 400)
			decode[map[string]any](t, f.call("POST", base+"/apply", f.token, "worker_one", "oversize", m.ApplyRequest{Batch: b, PreviewSHA256: strings.Repeat("a", 64)}), 400)
			resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: strings.Repeat(escaped, 4095) + "x", Evidence: make([]string, 10)}
			for index := range resolution.Evidence {
				resolution.Evidence[index] = strings.Repeat(escaped, 1023) + "x"
			}
			decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "oversize-resolution", resolution), 400)
			for _, table := range tables {
				if f.count(table) != before[table] {
					t.Fatal("rejected request changed retained state", table)
				}
			}
			decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 200)
			f.validate()
		})
	}
}

func TestEncodedRecordBoundariesRemainReadableInFullDetail(t *testing.T) {
	f := fresh(t)
	b := batch()
	original := b.Obligations[1]
	original.Summary = strings.Repeat("<", 5000)
	original.Summary += strings.Repeat("x", (32<<10)-len(m.Encode(original)))
	b.Obligations = []m.Obligation{original}
	f.apply(b, "boundary")
	id := f.page("?status=all").Items[0].ID
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: strings.Repeat("<", 2600), Evidence: []string{"x"}}
	resolution.Evidence[0] += strings.Repeat("x", (16<<10)-len(m.Encode(resolution)))
	decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "boundary-resolution", resolution), 200)
	response := f.call("GET", base+"/"+id, f.token, "", "", nil)
	detail := decode[m.Detail](t, response, 200)
	if len(m.Encode(detail.Original)) != 32<<10 || len(m.Encode(resolution)) != 16<<10 || response.Body.Len() > 64<<10 || detail.Original.Evidence == nil || len(detail.Original.Evidence) != 0 || detail.Resolution == nil {
		t.Fatal("full boundary detail exceeded client budget or changed source evidence", response.Body.Len())
	}
	f.validate()
}

func TestStoredResolutionRequiresEvidenceWhileOriginalMayBeEmpty(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "seed")
	var id string
	for _, item := range f.page("?status=all").Items {
		if item.SourceKey == "route-1" {
			id = item.ID
		}
	}
	detail := decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 200)
	if detail.Original.Evidence == nil || len(detail.Original.Evidence) != 0 || detail.Resolution != nil {
		t.Fatal("empty imported evidence was not preserved")
	}
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: "Verified receiving work", Evidence: []string{"commit:verified"}}
	decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "resolve", resolution), 200)
	detail = decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 200)
	if detail.Original.Evidence == nil || len(detail.Original.Evidence) != 0 || detail.Resolution == nil || len(detail.Resolution.Evidence) != 1 {
		t.Fatal("resolution changed imported evidence or lost native evidence")
	}
	f.validate()
	f.exec(`DROP TRIGGER review_adoption_resolutions_immutable`)
	f.exec(`UPDATE review_adoption_resolutions SET evidence_json='[]' WHERE obligation_id=?`, id)
	for _, statement := range h.SchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_resolutions_immutable ") {
			f.exec(statement)
		}
	}
	decode[map[string]any](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 500)
	if _, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3105); err == nil {
		t.Fatal("empty persisted resolution evidence silently cleared the hold")
	}
	if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("empty persisted resolution evidence passed snapshot validation")
	}
}

func TestStoredResolutionRetainsCombinedEncodedBudget(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "seed")
	id := f.page("?status=all").Items[0].ID
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: "Verified", Evidence: []string{"commit:verified"}}
	decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "resolve", resolution), 200)
	f.validate()
	f.exec(`DROP TRIGGER review_adoption_resolutions_immutable`)
	f.exec(`UPDATE review_adoption_resolutions SET reason=? WHERE obligation_id=?`, strings.Repeat("<", 4096), id)
	for _, statement := range h.SchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_resolutions_immutable ") {
			f.exec(statement)
		}
	}
	decode[map[string]any](t, f.call("GET", base+"/"+id, f.token, "", "", nil), 500)
	if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("stored resolution exceeded the combined encoded budget")
	}
}

func TestSnapshotCorruptionAndAbsenceAreFailClosed(t *testing.T) {
	for name, corrupt := range map[string]func(*fixture){"source": func(f *fixture) {
		f.exec(`UPDATE review_adoption_sources SET snapshot_sha256=?`, strings.Repeat("b", 64))
	}, "gap": func(f *fixture) { f.exec(`UPDATE review_adoption_sources SET revision=2`) }, "status": func(f *fixture) {
		f.exec(`UPDATE review_adoption_obligations SET status='resolved',revision=2 WHERE source_key='finding-1'`)
	}, "extra-target": func(f *fixture) {
		f.exec(`INSERT INTO review_adoption_targets SELECT id,project_id,123 FROM review_adoption_obligations WHERE source_key='finding-1'`)
	}, "schema": func(f *fixture) { f.exec(`DROP INDEX review_adoption_targets_scope`) }, "batch": func(f *fixture) {
		f.exec(`DROP TRIGGER review_adoption_imports_immutable`)
		f.exec(`UPDATE review_adoption_imports SET batch_sha256=?`, strings.Repeat("b", 64))
		f.exec(h.SchemaSQL()[12])
	}} {
		t.Run(name, func(t *testing.T) {
			f := fresh(t)
			f.apply(batch(), "seed")
			f.validate()
			corrupt(f)
			if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("corruption accepted")
			}
			if name == "status" || name == "extra-target" {
				if _, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); err == nil {
					t.Fatal("corrupt local scope silently cleared")
				}
			}
		})
	}
	f := fresh(t)
	f.apply(batch(), "seed")
	for _, query := range []string{`DELETE FROM review_adoption_targets`, `DELETE FROM review_adoption_obligations`, `UPDATE review_adoption_obligations SET original_json='{}'`, `UPDATE review_adoption_targets SET pull_number=2`} {
		if _, err := f.store.DB().Exec(query); err == nil {
			t.Fatal("immutable source changed")
		}
	}
	dir := filepath.Join(t.TempDir(), "old")
	old, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = h.ValidateSnapshot(t.Context(), old.DB()); err != nil {
		t.Fatal(err)
	}
	if open, err := h.HasOpen(t.Context(), old.DB(), "project_one", 1); err != nil || open {
		t.Fatal("genuine absence did not return zero", err)
	}
	if _, err := old.DB().Exec(`CREATE TABLE review_adoption_partial(id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadFacts(t.Context(), old.DB(), "project_one", 1); err == nil {
		t.Fatal("partial domain swallowed")
	}
}

func TestConcurrentExactReplayAndSourceCAS(t *testing.T) {
	f := fresh(t)
	b := batch()
	p := f.preview(b)
	request := m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	scope := f.scope()
	var wg sync.WaitGroup
	results := make(chan string, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := f.service.Apply(context.Background(), scope, "same-key", request)
			if err != nil {
				failures <- err
				return
			}
			results <- string(body)
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	first := ""
	for result := range results {
		if first == "" {
			first = result
		}
		if first != result {
			t.Fatal("concurrent receipt mismatch")
		}
	}
	if f.count("review_adoption_obligations") != 2 || f.count("review_adoption_imports") != 1 || f.count("receipts") != 1 {
		t.Fatal("duplicate apply")
	}
	one := int64(1)
	b.ExpectedRevision = &one
	b.Obligations = append(b.Obligations, m.Obligation{SourceKey: "next", Kind: "finding", Summary: "new", OriginalStatus: "pending", AffectedPullNumbers: []int64{4}, Evidence: []string{}})
	p = f.preview(b)
	request = m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	results = make(chan string, 2)
	failures = make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := f.service.Apply(context.Background(), scope, fmt.Sprint("key-", i), request)
			if err != nil {
				failures <- err
				return
			}
			results <- string(body)
		}(i)
	}
	wg.Wait()
	if len(results) != 1 || len(failures) != 1 {
		t.Fatal("source CAS not atomic")
	}
	f.validate()
}
func TestAdoptionObligationsMaximumBatchUsesOrdinaryWriteBudget(t *testing.T) {
	f := fresh(t)
	b := batch()
	b.Obligations = nil
	for i := 0; i < 100; i++ {
		o := m.Obligation{SourceKey: fmt.Sprintf("item-%03d", i), Kind: "finding", Summary: "inert", Evidence: []string{}, OriginalStatus: "pending"}
		for n := int64(1); n <= 100; n++ {
			o.AffectedPullNumbers = append(o.AffectedPullNumbers, n)
		}
		b.Obligations = append(b.Obligations, o)
	}
	preview := f.preview(b)
	input, err := json.Marshal(m.ApplyRequest{Batch: b, PreviewSHA256: preview.PreviewSHA256})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := f.call("POST", base+"/apply", f.token, "worker_one", "maximum", input)
	elapsed := time.Since(started)
	receipt := decode[m.Receipt](t, response, 200)
	t.Logf("100-obligation/10000-target HTTP apply completed in %s through ordinary core.Mutate deadline", elapsed)
	if receipt.New != 100 || f.count("review_adoption_targets") != 10000 {
		t.Fatal("maximum bounded batch incomplete")
	}
	facts, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 1)
	if err != nil || facts.Open != 100 {
		t.Fatal(facts, err)
	}
	f.validate()
}

func TestSnapshotRefusesResealedOriginalCorruption(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "resealed-original")
	f.validate()
	// Restore canonical trigger declarations after deliberately corrupting owned
	// synthetic rows, so logical validation must detect the conflicting fact.
	f.exec(`DROP TRIGGER review_adoption_originals_immutable`)
	var id, raw string
	if err := f.store.DB().QueryRow(`SELECT id,original_json FROM review_adoption_obligations LIMIT 1`).Scan(&id, &raw); err != nil {
		t.Fatal(err)
	}
	var original m.Obligation
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	original.Summary = "rewritten original"
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	f.exec(`UPDATE review_adoption_obligations SET original_json=?,original_sha256=? WHERE id=?`, string(body), hex.EncodeToString(sum[:]), id)
	for _, statement := range h.SchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_originals_immutable ") {
			f.exec(statement)
		}
	}
	if err = h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("resealed changed source fact accepted")
	}
}

func TestPreviewWorkerProjectBindingAndReadOnlyInventory(t *testing.T) {
	f := fresh(t)
	b := batch()
	p := f.preview(b)
	f.exec(`INSERT INTO workers VALUES('project_one','worker_other','Other')`)
	request := m.ApplyRequest{Batch: b, PreviewSHA256: p.PreviewSHA256}
	decode[map[string]any](t, f.call("POST", base+"/apply", f.token, "worker_other", "different-worker", request), 409)
	otherBase := strings.Replace(base, "project_one", "project_two", 1)
	decode[map[string]any](t, f.call("POST", otherBase+"/apply", f.token, "worker_two", "different-project", request), 409)
	decode[m.Receipt](t, f.call("POST", base+"/apply", f.token, "worker_one", "good", request), 200)
	beforeEvents, beforeReceipts := f.count("events"), f.count("receipts")
	page := f.page("?status=all&limit=1")
	decode[map[string]any](t, f.call("GET", otherBase+"?status=all&limit=1&cursor="+page.NextCursor, f.token, "", "", nil), 400)
	decode[map[string]any](t, f.call("GET", otherBase+"/"+page.Items[0].ID, f.token, "", "", nil), 404)
	if got := f.page("?pull_number=3251"); len(got.Items) != 1 || got.Items[0].SourceKey != "finding-1" {
		t.Fatal("PR selector leaked unrelated ledger")
	}
	_, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.ReadSource(t.Context(), f.store.DB(), "project_one", b.Source.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	f.validate()
	if f.count("events") != beforeEvents || f.count("receipts") != beforeReceipts {
		t.Fatal("reads mutated audit state")
	}
}
