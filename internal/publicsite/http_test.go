package publicsite

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestPublicationStrictHTTPAndScopedPreview(t *testing.T) {
	f := newPublicationFixture(t)
	manifest, _ := json.Marshal(testPublicManifest())
	base := f.base() + "/publication"
	for _, body := range []string{
		`{"expected_revision":0,"enabled":true,"manifest":null}`,
		`{"expected_revision":null,"enabled":true,"manifest":` + string(manifest) + `}`,
		`{"expected_revision":0,"enabled":null,"manifest":` + string(manifest) + `}`,
		`{"expected_revision":0,"enabled":true,"manifest":` + string(manifest) + `,"preview_sha256":null}`,
		`{"expected_revision":0,"enabled":true,"manifest":` + string(manifest) + `,"extra":"private-sentinel"}`,
		`{"expected_revision":0,"expected_revision":1,"enabled":true,"manifest":` + string(manifest) + `}`,
		`{"expected_revision":0,"enabled":true,"manifest":{"slug":"showcase","presentation":{"name":"Approved"},"private_brief":"private-sentinel"}}`,
	} {
		r := httptest.NewRequest("POST", base+"/preview", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("X-FC-Worker", f.worker)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private-sentinel") {
			t.Fatal("strict request", w.Code)
		}
	}
	input := Approved{0, true, testPublicManifest()}
	preview := f.prepare(t, input)
	body := map[string]any{"expected_revision": 0, "enabled": true, "manifest": input.Manifest, "preview_sha256": preview.SHA256}
	changed := input.Manifest
	changed.Description = "Changed after preview"
	body["manifest"] = changed
	if w := f.call(t, "PUT", base, f.admin, "stale-preview", body); w.Code != 409 {
		t.Fatal("changed preview", w.Code)
	}
	body["manifest"] = input.Manifest
	first := f.call(t, "PUT", base, f.admin, "publish", body)
	if first.Code != 200 {
		t.Fatal(first.Code)
	}
	body["manifest"] = changed
	changedPreview := f.prepare(t, Approved{1, true, changed})
	body["expected_revision"] = 1
	body["preview_sha256"] = changedPreview.SHA256
	if w := f.call(t, "PUT", base, f.admin, "publish", body); w.Code != 409 || !strings.Contains(w.Body.String(), "idempotency_conflict") {
		t.Fatal("changed key", w.Code, w.Body.String())
	}
	if w := f.call(t, "PUT", base, f.reader, "reader-write", body); w.Code != 403 {
		t.Fatal("project writer changed public policy", w.Code)
	}
	oldProject, oldWorker := f.project, f.worker
	f.project, f.worker = f.other, ""
	w := f.call(t, "POST", f.base()+"/workers", f.admin, "other-worker", map[string]string{"name": "Other private worker"})
	var worker struct {
		ID string `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &worker) != nil {
		t.Fatal("other worker setup")
	}
	f.worker = worker.ID
	// Same slug is an opaque conflict; no owning project or source is exposed.
	otherPreview := f.prepare(t, input)
	body = map[string]any{"expected_revision": 0, "enabled": true, "manifest": input.Manifest, "preview_sha256": otherPreview.SHA256}
	w = f.call(t, "PUT", f.base()+"/publication", f.admin, "other-publish", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "publication_slug_conflict") || strings.Contains(w.Body.String(), oldProject) {
		t.Fatal("slug conflict disclosure", w.Code, w.Body.String())
	}
	body["preview_sha256"] = preview.SHA256
	if w = f.call(t, "PUT", f.base()+"/publication", f.admin, "wrong-project-preview", body); w.Code != 409 {
		t.Fatal("cross-project preview binding", w.Code)
	}
	f.project, f.worker = oldProject, oldWorker
}

func readPublicFrame(reader *bufio.Reader) (string, error) {
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		frame.WriteString(line)
		if line == "\n" {
			return frame.String(), nil
		}
	}
}

func TestPublicStreamReplayQuietHintsAndUnpublish(t *testing.T) {
	f := newPublicationFixture(t)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/public/projects/showcase/events?cursor="+first.Cursor, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("stream headers", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	frame, err := readPublicFrame(reader)
	if err != nil || frame != ": ready\n\n" {
		t.Fatal("quiet readiness", err)
	}
	// A same-content hint is followed by a genuine change. The next data frame
	// must describe only revision 2, never a duplicate frame for revision 1.
	f.refresh(t)
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return core.MarkPublicProjectionChanged(ctx, f.project) }); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 1
	input.Manifest.Description = "Live public description"
	if w := f.apply(t, input, "live-update"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	frame, err = readPublicFrame(reader)
	if err != nil || !strings.Contains(frame, "event: public-project\n") || !strings.Contains(frame, `"revision":2`) || strings.Contains(frame, f.project) || strings.Contains(frame, "snapshot_id") {
		t.Fatal("public delivery", frame, err)
	}
	if w := f.call(t, "POST", f.base()+"/publication/unpublish", f.admin, "unpublish", map[string]int64{"expected_revision": 2}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	frame, err = readPublicFrame(reader)
	if err != nil || !strings.Contains(frame, `"code":"not_found"`) {
		t.Fatal("unpublish termination", frame, err)
	}
	if _, err = reader.ReadByte(); err != io.EOF {
		t.Fatal("stream did not close", err)
	}
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase/events?cursor="+first.Cursor, "", "", nil); w.Code != 404 {
		t.Fatal("unpublished replay", w.Code)
	}
}

func TestPublicStreamReplayAfterSubscribeAndGeneration(t *testing.T) {
	f := newPublicationFixture(t)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	input.ExpectedRevision = 1
	input.Manifest.Description = "Recorded before subscribing"
	if w := f.apply(t, input, "second"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	server := httptest.NewServer(f.mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/public/projects/showcase/events?cursor="+first.Cursor, nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if _, err = readPublicFrame(reader); err != nil {
		t.Fatal(err)
	}
	frame, err := readPublicFrame(reader)
	if err != nil || !strings.Contains(frame, `"revision":2`) {
		t.Fatal("durable replay", err, frame)
	}
	response.Body.Close()
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase/events?cursor="+first.Generation+":3", "", "", nil); w.Code != 409 {
		t.Fatal("future cursor", w.Code)
	}
	// No hidden browser or credential mutation occurs on anonymous reads.
	before := publicationCounts(t, f.store)
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase", f.reader, "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if !reflectCounts(before, publicationCounts(t, f.store)) {
		t.Fatal("public read mutated private state")
	}
}

func TestPublicProjectionOverflowPreservesLastGood(t *testing.T) {
	f := newPublicationFixture(t)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	rounds := make([]RoundFacts, MaxChannelRounds+1)
	for i := range rounds {
		rounds[i] = RoundFacts{Key: "public_round_" + strings.Repeat("a", 30) + hexByte(i), Completed: true, Finalized: true, Complete: true}
	}
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "open", Hosted: rounds}}, false)
	f.refresh(t)
	if after := f.public(t); after.Cursor != first.Cursor {
		t.Fatal("overflow changed projection")
	}
	w := f.call(t, "GET", f.base()+"/publication", f.admin, "", nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"refresh_error":"projection_unavailable"`)) {
		t.Fatal("overflow diagnostic", w.Code, w.Body.String())
	}
}

func hexByte(i int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[i/16], digits[i%16]})
}

func TestPublicationPreviewRequiresRoomForApplyWrapper(t *testing.T) {
	f := newPublicationFixture(t)
	var reads atomic.Int64
	f.service.reader = func(context.Context, *sql.Tx, string, Manifest, string) ([]ReviewFacts, error) {
		reads.Add(1)
		return nil, nil
	}
	input := Approved{0, true, testPublicManifest()}
	input.Manifest.ProjectMap = []Card{}
	for i := 0; i < 15; i++ {
		input.Manifest.ProjectMap = append(input.Manifest.ProjectMap, Card{Key: "card-" + strconv.Itoa(i), Title: "Approved", Description: strings.Repeat("\u2028", 2730), Links: []Link{}})
	}
	input.Manifest.ProjectMap = append(input.Manifest.ProjectMap, Card{Key: "ascii", Title: "Approved", Description: strings.Repeat("x", 8192), Links: []Link{}}, Card{Key: "last", Title: "Approved", Links: []Link{}})
	base, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	remaining := (256 << 10) - 1 - len(base)
	if remaining < 0 || remaining > 8192 {
		t.Fatal("near-bound fixture shape", remaining)
	}
	input.Manifest.ProjectMap[len(input.Manifest.ProjectMap)-1].Description = strings.Repeat("x", remaining)
	before := publicationCounts(t, f.store)
	w := f.call(t, "POST", f.base()+"/publication/preview", f.admin, "", input)
	if w.Code != 400 || reads.Load() != 0 || !reflectCounts(before, publicationCounts(t, f.store)) {
		t.Fatal("preview admitted unapplicable wrapper or read source", w.Code, reads.Load())
	}
	input.Manifest.ProjectMap[len(input.Manifest.ProjectMap)-1].Description = strings.Repeat("x", remaining-1024)
	if w = f.apply(t, input, "near-bound-apply"); w.Code != 200 {
		t.Fatal("nearby valid preview/apply failed", w.Code, w.Body.String())
	}
}
