package publicsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/server"
)

type publicationFixture struct {
	store                                 *core.Store
	service                               *Service
	mux                                   *http.ServeMux
	admin, worker, project, other, reader string
	mu                                    sync.Mutex
	facts                                 []ReviewFacts
	fail                                  bool
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &publicationFixture{store: store, admin: strings.TrimSpace(string(secret)), reader: "private-reader-sentinel"}
	var repository int64 = 990
	f.mux = server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		repository++
		return server.RepositoryIdentity{ID: repository, FullName: "synthetic/project"}, nil
	}))
	f.service = New(store, func(context.Context, *sql.Tx, string, Manifest, string) ([]ReviewFacts, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			return nil, errors.New("private-provider-failure-sentinel")
		}
		data, _ := json.Marshal(f.facts)
		var out []ReviewFacts
		json.Unmarshal(data, &out)
		return out, nil
	})
	f.service.Register(f.mux, t.Context())
	create := func(name string) string {
		w := f.call(t, "POST", "/api/v1/projects", f.admin, "setup-"+name, map[string]any{"name": name, "repository": "synthetic/" + name})
		var v struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("project setup", w.Code)
		}
		return v.ID
	}
	f.project = create("one")
	f.other = create("two")
	w := f.call(t, "POST", f.base()+"/workers", f.admin, "worker", map[string]string{"name": "Private worker sentinel"})
	var worker struct {
		ID string `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &worker) != nil {
		t.Fatal("worker setup")
	}
	f.worker = worker.ID
	digest := sha256.Sum256([]byte(f.reader))
	w = f.call(t, "POST", f.base()+"/credentials", f.admin, "reader", map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "permissions": []string{"read", "write"}})
	if w.Code != 200 {
		t.Fatal("grant setup", w.Code)
	}
	return f
}
func (f *publicationFixture) base() string { return "/api/v1/projects/" + f.project }
func (f *publicationFixture) call(t *testing.T, method, path, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data)).WithContext(t.Context())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		if f.worker != "" && strings.HasPrefix(path, f.base()+"/") {
			r.Header.Set("X-FC-Worker", f.worker)
		}
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}
func testPublicManifest() Manifest {
	manifest, err := NormalizeManifest(Manifest{Slug: "showcase", Presentation: presentationmodel.Config{Name: "Approved <literal> project"}, Description: "Approved public description", Reviews: []EnrolledReview{{Number: 7, Title: "Approved PR title", URL: "https://github.com/synthetic/project/pull/7"}}})
	if err != nil {
		panic("invalid synthetic manifest")
	}
	return manifest
}
func (f *publicationFixture) prepare(t *testing.T, input Approved) Preview {
	t.Helper()
	w := f.call(t, "POST", f.base()+"/publication/preview", f.admin, "", input)
	var out Preview
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("preview", w.Code, w.Body.String())
	}
	return out
}
func (f *publicationFixture) apply(t *testing.T, input Approved, key string) *httptest.ResponseRecorder {
	t.Helper()
	preview := f.prepare(t, input)
	return f.call(t, "PUT", f.base()+"/publication", f.admin, key, map[string]any{"expected_revision": input.ExpectedRevision, "enabled": input.Enabled, "manifest": input.Manifest, "preview_sha256": preview.SHA256})
}
func (f *publicationFixture) public(t *testing.T) Snapshot {
	t.Helper()
	w := f.call(t, "GET", "/api/v1/public/projects/showcase", "", "", nil)
	var out Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("public snapshot", w.Code, w.Body.String())
	}
	return out
}
func (f *publicationFixture) setFacts(facts []ReviewFacts, fail bool) {
	f.mu.Lock()
	f.facts = facts
	f.fail = fail
	f.mu.Unlock()
}
func (f *publicationFixture) refresh(t *testing.T) {
	t.Helper()
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return Refresh(ctx, tx, f.project, f.service.reader) }); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationPreviewPrivacyReplayAndGeneration(t *testing.T) {
	f := newPublicationFixture(t)
	base := f.base() + "/publication"
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase", "", "", nil); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("default public exposure", w.Code)
	}
	if w := f.call(t, "GET", base, f.reader, "", nil); w.Code != 403 {
		t.Fatal("nonadmin configuration", w.Code)
	}
	before := publicationCounts(t, f.store)
	input := Approved{0, true, testPublicManifest()}
	preview := f.prepare(t, input)
	if !reflectCounts(before, publicationCounts(t, f.store)) {
		t.Fatal("preview mutated storage")
	}
	applyBody := map[string]any{"expected_revision": input.ExpectedRevision, "enabled": input.Enabled, "manifest": input.Manifest, "preview_sha256": preview.SHA256}
	first := f.call(t, "PUT", base, f.admin, "publish-first", applyBody)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	current := f.public(t)
	if current.Revision != 1 || current.Content.Presentation.Name != "Approved <literal> project" || current.Content.Reviews[0].Lifecycle != "unknown" {
		t.Fatal("published content", current)
	}
	for _, private := range []string{f.project, f.worker, f.store.InstanceID(), f.admin, f.reader, "round_salt", "snapshot_id", "private-provider"} {
		raw, _ := json.Marshal(current)
		if strings.Contains(string(raw), private) {
			t.Fatal("private anonymous value", private)
		}
	}
	input.ExpectedRevision = 1
	input.Manifest.Description = "Changed approved description"
	if w := f.apply(t, input, "publish-second"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	second := f.public(t)
	if second.Revision != 2 || second.Generation != current.Generation {
		t.Fatal("editorial revision")
	}
	replay := f.call(t, "PUT", base, f.admin, "publish-first", applyBody)
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("lost response replay", replay.Code)
	}
	if got := f.public(t); got.Revision != 2 {
		t.Fatal("replay advanced projection")
	}
	input.ExpectedRevision = 2
	if w := f.apply(t, input, "publish-noop"); w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"unchanged"`) {
		t.Fatal("no-op", w.Code, w.Body.String())
	}
	if got := f.public(t); got.Revision != 2 || got.PublishedAt != second.PublishedAt {
		t.Fatal("no-op freshness")
	}
	if w := f.call(t, "POST", base+"/unpublish", f.admin, "hide", map[string]int64{"expected_revision": 2}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase", f.admin, "", nil); w.Code != 404 {
		t.Fatal("credential bypassed unpublish", w.Code)
	}
	input.ExpectedRevision = 3
	if w := f.apply(t, input, "republish"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	third := f.public(t)
	if third.Generation == second.Generation || third.Revision != 1 {
		t.Fatal("new generation")
	}
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase/history/1?generation="+second.Generation, "", "", nil); w.Code != 409 {
		t.Fatal("old generation history", w.Code)
	}
}

func publicationCounts(t *testing.T, store *core.Store) [4]int {
	t.Helper()
	var out [4]int
	for i, table := range []string{"project_publications", "public_project_snapshots", "receipts", "events"} {
		if err := store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func reflectCounts(a, b [4]int) bool { return a == b }

func TestPublicationReviewProjectionConservationFailureAndEnrollment(t *testing.T) {
	f := newPublicationFixture(t)
	round := RoundFacts{Key: "public_round_" + strings.Repeat("a", 32), Completed: true, Finalized: true, Complete: true, Found: 1, Accepted: 1, ObservedAt: "2026-01-01T00:00:00Z"}
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", Hosted: []RoundFacts{round}}}, false)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	first := f.public(t)
	hints, release, err := f.store.SubscribePublicProjection(f.project, "testclient")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.refresh(t)
	if next := f.public(t); next.Cursor != first.Cursor || next.PublishedAt != first.PublishedAt {
		t.Fatal("unchanged facts changed freshness")
	}
	select {
	case <-hints:
		t.Fatal("unchanged projection emitted hint")
	default:
	}
	f.setFacts(nil, false)
	f.refresh(t)
	if next := f.public(t); next.Cursor != first.Cursor || next.Content.Reviews[0].Lifecycle != "merged" || len(next.Content.Reviews[0].Hosted) != 1 {
		t.Fatal("private removal erased published history")
	}
	secondRound := round
	secondRound.Key = "public_round_" + strings.Repeat("b", 32)
	secondRound.Found = 0
	secondRound.Accepted = 0
	secondRound.ObservedAt = "2026-01-02T00:00:00Z"
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", Hosted: []RoundFacts{secondRound}}}, false)
	f.refresh(t)
	second := f.public(t)
	if second.Revision != 2 || len(second.Content.Reviews[0].Hosted) != 2 {
		t.Fatal("new round and retained round")
	}
	select {
	case <-hints:
	default:
		t.Fatal("changed projection missing hint")
	}
	f.setFacts(nil, true)
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='authoritative private write' WHERE id=?`, f.project); err != nil {
			return err
		}
		return Refresh(ctx, tx, f.project, f.service.reader)
	}); err != nil {
		t.Fatal("projection failure blocked authoritative write", err)
	}
	var name, code string
	f.store.DB().QueryRow(`SELECT name FROM projects WHERE id=?`, f.project).Scan(&name)
	f.store.DB().QueryRow(`SELECT refresh_error FROM project_publications WHERE project_id=?`, f.project).Scan(&code)
	if name != "authoritative private write" || code != "projection_unavailable" {
		t.Fatal("contained failure evidence", name, code)
	}
	if got := f.public(t); got.Cursor != second.Cursor || got.PublishedAt != second.PublishedAt {
		t.Fatal("failure changed last good projection")
	}
	select {
	case <-hints:
		t.Fatal("failed projection emitted hint")
	default:
	}
	f.setFacts(nil, false)
	f.refresh(t)
	if w := f.call(t, "POST", f.base()+"/publication/unpublish", f.admin, "hide", map[string]int64{"expected_revision": 1}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	input.ExpectedRevision = 2
	if w := f.apply(t, input, "new-generation"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := f.public(t); got.Generation == second.Generation || got.Content.Reviews[0].Lifecycle != "unknown" || len(got.Content.Reviews[0].Hosted) != 0 {
		t.Fatal("old generation facts republished")
	}
}

func TestPublicationHistoryRetentionAndInactiveGate(t *testing.T) {
	f := newPublicationFixture(t)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	input.ExpectedRevision = 1
	input.Manifest.Description = "Second description"
	if w := f.apply(t, input, "second"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := f.call(t, "GET", "/api/v1/public/projects/showcase/history?limit=1", "", "", nil)
	var page History
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || !page.HasMore || page.Items[0].Revision != 2 {
		t.Fatal("history first", w.Code, w.Body.String())
	}
	w = f.call(t, "GET", "/api/v1/public/projects/showcase/history?limit=1&cursor="+page.Cursor, "", "", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.HasMore || len(page.Items) != 1 || page.Items[0].Revision != 1 {
		t.Fatal("history continuation", w.Code, w.Body.String())
	}
	if w = f.call(t, "GET", "/api/v1/public/projects/showcase/history/1?generation="+first.Generation, "", "", nil); w.Code != 200 {
		t.Fatal("history detail", w.Code)
	}
	if _, err := f.store.DB().Exec(`UPDATE public_project_snapshots SET created_at=? WHERE revision=1`, time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if w = f.call(t, "GET", "/api/v1/public/projects/showcase/history/1?generation="+first.Generation, "", "", nil); w.Code != 404 {
		t.Fatal("expired history", w.Code)
	}
	if w = f.call(t, "GET", "/api/v1/public/projects/showcase/events?cursor="+first.Cursor, "", "", nil); w.Code != 409 {
		t.Fatal("expired stream cursor", w.Code)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO metadata(key,value) VALUES('restore:state','inactive')`); err != nil {
		t.Fatal(err)
	}
	if w = f.call(t, "GET", "/api/v1/public/projects/showcase", "", "", nil); w.Code != 404 {
		t.Fatal("inactive restore served", w.Code)
	}
	if _, err := f.store.DB().Exec(`UPDATE metadata SET value='coordination_active' WHERE key='restore:state'`); err != nil {
		t.Fatal(err)
	}
	if w = f.call(t, "GET", "/api/v1/public/projects/showcase", "", "", nil); w.Code != 200 {
		t.Fatal("activated stored read", w.Code)
	}
}

func TestPublicationRetentionKeepsBoundedHistoryAndCurrent(t *testing.T) {
	for _, ageOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "age"}[ageOut], func(t *testing.T) {
			f := newPublicationFixture(t)
			if w := f.apply(t, Approved{0, true, testPublicManifest()}, "publish"); w.Code != 200 {
				t.Fatal(w.Code)
			}
			first := f.public(t)
			// A bounded synthetic retained-history fixture avoids making a thousand
			// unrelated HTTP edits merely to reach the maintenance boundary.
			if _, err := f.store.DB().Exec(`WITH RECURSIVE revisions(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM revisions WHERE n<1001) INSERT INTO public_project_snapshots(project_id,generation,revision,content_json,created_at) SELECT s.project_id,s.generation,n,s.content_json,s.created_at FROM revisions JOIN public_project_snapshots s ON s.project_id=? AND s.generation=? AND s.revision=1`, f.project, first.Generation); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().Exec(`UPDATE project_publications SET public_revision=1001 WHERE project_id=?`, f.project); err != nil {
				t.Fatal(err)
			}
			if ageOut {
				if _, err := f.store.DB().Exec(`UPDATE public_project_snapshots SET created_at=? WHERE revision<1001`, time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			}
			f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "open"}}, false)
			f.refresh(t)
			current := f.public(t)
			if current.Revision != 1002 {
				t.Fatal("retention changed revision")
			}
			var count, minimum int
			if err := f.store.DB().QueryRow(`SELECT count(*),min(revision) FROM public_project_snapshots WHERE project_id=?`, f.project).Scan(&count, &minimum); err != nil {
				t.Fatal(err)
			}
			if !ageOut && (count != 1000 || minimum != 3) || ageOut && (count != 2 || minimum != 1001) {
				t.Fatal("retained history bound", count, minimum)
			}
		})
	}
}
