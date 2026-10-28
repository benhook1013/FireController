package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/server"
)

func presentationClientFixture(t *testing.T) *mutationFixture {
	t.Helper()
	f := newMutationFixture(t)
	if e := presentation.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	presentation.Register(mux, f.store)
	f.handler = mux
	return f
}
func presentationClientCounts(t *testing.T, f *mutationFixture) []int {
	t.Helper()
	out := make([]int, 4)
	for i, table := range []string{"events", "receipts", "project_presentations", "project_presentation_history"} {
		if e := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); e != nil {
			t.Fatal(e)
		}
	}
	return out
}
func presentationClientError(t *testing.T, err error, code string, unknown bool) *client.Error {
	t.Helper()
	var e *client.Error
	if !errors.As(err, &e) || e.Code != code || e.OutcomeUnknown != unknown {
		t.Fatalf("wanted %s unknown%v: %#v", code, unknown, err)
	}
	return e
}
func TestPresentationClientActualAPIHistoryReplayRestartAndLostReceipt(t *testing.T) {
	f := presentationClientFixture(t)
	var lose atomic.Bool
	lose.Store(false)
	var mutations atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/presentation") && r.Method == "PUT" {
			mutations.Add(1)
			if r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("binding lost")
			}
			if lose.CompareAndSwap(true, false) {
				record := httptest.NewRecorder()
				f.handler.ServeHTTP(record, r)
				if record.Code != 200 {
					t.Errorf("not committed: %d", record.Code)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	})
	service := httptest.NewServer(handler)
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	before := presentationClientCounts(t, f)
	s, e := c.Presentation(t.Context())
	if e != nil || s.Presentation.Revision != 0 || s.Presentation.Name != "project" || s.EventCursor == "" {
		t.Fatal(s, e)
	}
	if !reflect.DeepEqual(before, presentationClientCounts(t, f)) {
		t.Fatal("implicit read wrote")
	}
	noop, err := c.ReplacePresentation(t.Context(), "default-noop", client.ReplacePresentationRequest{Presentation: client.PresentationConfig{Name: "project"}})
	if err != nil || noop.Revision != 0 || noop.Outcome != "unchanged" {
		t.Fatal("default noop", noop, err)
	}
	readToken := "presentation-reader"
	sum := sha256.Sum256([]byte(readToken))
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	reader := f.newClient(t, readToken, f.sender, service.URL)
	if _, e := reader.Presentation(t.Context()); e != nil {
		t.Fatal(e)
	}
	_, err = reader.ReplacePresentation(t.Context(), "forbidden", client.ReplacePresentationRequest{Presentation: client.PresentationConfig{Name: "Forbidden"}})
	presentationClientError(t, err, "forbidden", false)
	lose.Store(true)
	attemptsBefore := mutations.Load()
	req := client.ReplacePresentationRequest{Presentation: client.PresentationConfig{Name: "  Named <literal>  ", Headings: map[string]string{"jobs": "Work"}, WorkerOrder: []string{f.sender}, ReviewGroups: []presentationmodel.ReviewGroup{{Title: "Foundation", PullNumbers: []int64{2, 7}}}}}
	_, e = c.ReplacePresentation(t.Context(), "original-key", req)
	presentationClientError(t, e, "invalid_response", true)
	if mutations.Load() != attemptsBefore+1 {
		t.Fatal("hidden retry")
	}
	before = presentationClientCounts(t, f)
	receipt, e := c.ReplacePresentation(t.Context(), "original-key", req)
	if e != nil || receipt.Revision != 1 || receipt.Outcome != "updated" || receipt.ProjectID != f.project || !reflect.DeepEqual(before, presentationClientCounts(t, f)) {
		t.Fatal(receipt, e)
	}
	req2 := client.ReplacePresentationRequest{ExpectedRevision: 1, Presentation: client.PresentationConfig{Name: "Later"}}
	if _, e = c.ReplacePresentation(t.Context(), "later-key", req2); e != nil {
		t.Fatal(e)
	}
	replay, e := c.ReplacePresentation(t.Context(), "original-key", req)
	if e != nil || replay != receipt {
		t.Fatal(replay, e)
	}
	old, e := c.PresentationRevision(t.Context(), 1)
	if e != nil || old.State.Name != "Named <literal>" || !reflect.DeepEqual(old.State.WorkerOrder, []string{f.sender}) || old.State.Headings["jobs"] != "Work" || len(old.State.ReviewGroups) != 1 || !reflect.DeepEqual(old.State.ReviewGroups[0].PullNumbers, []int64{2, 7}) {
		t.Fatal(old, e)
	}
	page, e := c.PresentationHistory(t.Context(), client.PageOptions{Limit: 1})
	if e != nil || len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore {
		t.Fatal(page, e)
	}
	next, e := c.PresentationHistory(t.Context(), client.PageOptions{Limit: 1, Cursor: page.Cursor})
	if e != nil || len(next.Items) != 1 || next.Items[0].Revision != 1 || next.HasMore {
		t.Fatal(next, e)
	}
	before = presentationClientCounts(t, f)
	if _, e = c.Presentation(t.Context()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, presentationClientCounts(t, f)) {
		t.Fatal("read effects")
	}
	job, e := c.CreateJob(t.Context(), "job-without-theme", client.CreateJobRequest{Title: "Job", Brief: "Brief", WorkerID: f.sender})
	if e != nil {
		t.Fatal(e)
	}
	ordinary, e := c.Job(t.Context(), job.ID)
	raw, _ := json.Marshal(ordinary)
	if e != nil || strings.Contains(string(raw), `"theme"`) || strings.Contains(string(raw), `"brand_name"`) {
		t.Fatal("appearance leaked into ordinary context", e)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatal("presentation contacted provider")
	}
	c.Close()
	service.Close()
	dir := filepath.Dir(f.tokenFile)
	if e = f.store.Close(); e != nil {
		t.Fatal(e)
	}
	f.store, e = core.Open(t.Context(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer f.store.Close()
	mux := server.New(f.store)
	jobs.Register(mux, f.store)
	inbox.Register(mux, f.store)
	presentation.Register(mux, f.store)
	f.handler = mux
	restarted := httptest.NewServer(f.handler)
	defer restarted.Close()
	c = f.newClient(t, f.admin, f.sender, restarted.URL)
	before = presentationClientCounts(t, f)
	replay, e = c.ReplacePresentation(t.Context(), "original-key", req)
	if e != nil || replay != receipt || !reflect.DeepEqual(before, presentationClientCounts(t, f)) {
		t.Fatal("restart replay", replay, e)
	}
}

func TestPresentationClientReadsRetainedRevisionZeroNameAndRepairsExplicitly(t *testing.T) {
	f := presentationClientFixture(t)
	legacyName := " \x01Legacy\tproject\n "
	if _, err := f.store.DB().Exec(`UPDATE projects SET name=? WHERE id=?`, legacyName, f.project); err != nil {
		t.Fatal(err)
	}
	service := httptest.NewServer(f.handler)
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	before := presentationClientCounts(t, f)
	snapshot, err := c.Presentation(t.Context())
	if err != nil || snapshot.Presentation.Revision != 0 || snapshot.Presentation.Name != legacyName || snapshot.Presentation.BrandName != "FireController" || snapshot.Presentation.Tagline != "Project workspace" || snapshot.EventCursor == "" {
		t.Fatalf("retained revision-zero snapshot=%+v err=%v", snapshot, err)
	}
	if !reflect.DeepEqual(before, presentationClientCounts(t, f)) {
		t.Fatal("revision-zero read repaired or wrote retained state")
	}

	receipt, err := c.ReplacePresentation(t.Context(), "explicit-repair", client.ReplacePresentationRequest{ExpectedRevision: 0, Presentation: client.PresentationConfig{Name: "Repaired project"}})
	if err != nil || receipt.Revision != 1 || receipt.Outcome != "updated" || receipt.ProjectID != f.project {
		t.Fatalf("repair receipt=%+v err=%v", receipt, err)
	}
	repaired, err := c.Presentation(t.Context())
	if err != nil || repaired.Presentation.Revision != 1 || repaired.Presentation.Name != "Repaired project" {
		t.Fatalf("repaired snapshot=%+v err=%v", repaired, err)
	}
	revision, err := c.PresentationRevision(t.Context(), 1)
	if err != nil || revision.Revision != 1 || revision.State.Revision != 1 || revision.State.Name != "Repaired project" {
		t.Fatalf("revision one=%+v err=%v", revision, err)
	}
}

func TestPresentationClientStrictControlledResponses(t *testing.T) {
	base := map[string]any{"presentation": map[string]any{"project_id": "project-one", "revision": 0, "name": "Project", "brand_name": "FireController", "tagline": "Project workspace", "headings": map[string]string{}, "theme": map[string]string{}, "updated_at": ""}, "event_cursor": "opaque-cursor"}
	for _, mutate := range []func(map[string]any){func(v map[string]any) { delete(v, "event_cursor") }, func(v map[string]any) { v["event_cursor"] = nil }, func(v map[string]any) { v["event_cursor"] = " padded" }, func(v map[string]any) { v["extra"] = true }, func(v map[string]any) { v["presentation"].(map[string]any)["project_id"] = "foreign" }, func(v map[string]any) { delete(v["presentation"].(map[string]any), "theme") }, func(v map[string]any) { v["presentation"].(map[string]any)["headings"] = nil }, func(v map[string]any) {
		v["presentation"].(map[string]any)["theme"] = map[string]string{"unknown": "#000000"}
	}, func(v map[string]any) {
		p := v["presentation"].(map[string]any)
		p["revision"] = 1
		p["updated_at"] = "2026-01-01T00:00:00Z"
		p["theme"] = map[string]string{"canvas_text": "#e8e7e4"}
	}, func(v map[string]any) { v["presentation"].(map[string]any)["name"] = nil }, func(v map[string]any) { v["presentation"].(map[string]any)["name"] = "" }, func(v map[string]any) { v["presentation"].(map[string]any)["name"] = strings.Repeat("x", 201) }, func(v map[string]any) { v["presentation"].(map[string]any)["name"] = 42 }, func(v map[string]any) {
		p := v["presentation"].(map[string]any)
		p["revision"] = 1
		p["updated_at"] = "2026-01-01T00:00:00Z"
		p["name"] = " Project "
	}} {
		raw, _ := json.Marshal(base)
		var v map[string]any
		json.Unmarshal(raw, &v)
		mutate(v)
		c, _ := progressResponseClient(t, progressJSON(t, v))
		_, e := c.Presentation(t.Context())
		presentationClientError(t, e, "invalid_response", false)
	}
	invalidUTF8 := `{"presentation":{"project_id":"project-one","revision":0,"name":"` + string([]byte{0xff}) + `","brand_name":"FireController","tagline":"Project workspace","headings":{},"theme":{},"updated_at":""},"event_cursor":"opaque-cursor"}`
	c, _ := progressResponseClient(t, invalidUTF8)
	_, e := c.Presentation(t.Context())
	presentationClientError(t, e, "invalid_response", false)
	for _, body := range []string{`{"items":null,"has_more":false}`, `{"items":[],"has_more":true}`, `{"items":[],"has_more":false,"cursor":"extra"}`, `{"items":[{"revision":1,"actor_id":"p","created_at":"bad"}],"has_more":false}`, `{"items":[{"revision":1,"actor_id":"p","created_at":"2026-01-01T00:00:00Z"},{"revision":2,"actor_id":"p","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`} {
		c, _ := progressResponseClient(t, body)
		_, e := c.PresentationHistory(t.Context(), client.PageOptions{})
		presentationClientError(t, e, "invalid_response", false)
	}
	// Controlled invalid success receipts cannot assert that a committed write failed.
	for _, body := range []string{`{"project_id":"foreign","revision":1,"outcome":"updated"}`, `{"project_id":"project-one","revision":0,"outcome":"updated"}`, `{"project_id":"project-one","revision":1}`, `{"project_id":"project-one","revision":null,"outcome":"updated"}`} {
		c, _ := progressResponseClient(t, body)
		_, e := c.ReplacePresentation(t.Context(), "k", client.ReplacePresentationRequest{Presentation: client.PresentationConfig{Name: "Name"}})
		presentationClientError(t, e, "invalid_response", true)
	}
}
func TestPresentationClientLocalValidationAndCancellation(t *testing.T) {
	c, calls := progressResponseClient(t, `{}`)
	for _, req := range []client.ReplacePresentationRequest{{ExpectedRevision: -1, Presentation: client.PresentationConfig{Name: "Name"}}, {ExpectedRevision: math.MaxInt64, Presentation: client.PresentationConfig{Name: "Name"}}, {Presentation: client.PresentationConfig{Name: ""}}, {Presentation: client.PresentationConfig{Name: "Name", Theme: map[string]string{"unknown": "#000000"}}}} {
		if _, e := c.ReplacePresentation(t.Context(), "k", req); e == nil {
			t.Fatal("invalid accepted")
		}
	}
	if _, e := c.PresentationRevision(t.Context(), 0); e == nil {
		t.Fatal("zero revision accepted")
	}
	if _, e := c.PresentationHistory(t.Context(), client.PageOptions{Limit: 101}); e == nil {
		t.Fatal("bad limit accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid local calls contacted HTTP")
	}
	entered := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer service.Close()
	c = newClient(t, service)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, e := c.Presentation(ctx); done <- e }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	cancel()
	select {
	case e := <-done:
		presentationClientError(t, e, "canceled", false)
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
}
