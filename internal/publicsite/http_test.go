package publicsite

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	rounds := make([]RoundFacts, 2*MaxChannelRounds+1)
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

func serviceWindowRounds(start, count int) []RoundFacts {
	rounds := make([]RoundFacts, count)
	for i := range rounds {
		n := start + i
		rounds[i] = RoundFacts{Key: fmt.Sprintf("public_round_%032x", n), Completed: true, Finalized: true, Complete: true, ObservedAt: time.Date(2026, 1, 1, 0, 0, n, 0, time.UTC).Format(time.RFC3339Nano)}
	}
	return rounds
}

func TestPublicationWindowPreservesLegacyBytesOmissionAndQuietFreshness(t *testing.T) {
	f := newPublicationFixture(t)
	cli := serviceWindowRounds(1, 100)
	hosted := serviceWindowRounds(1000, 1)
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", CLI: cli, Hosted: hosted}}, false)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "legacy-full-window"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	oldPath := fmt.Sprintf("/api/v1/public/projects/showcase/history/1?generation=%s", first.Generation)
	oldBytes := append([]byte(nil), f.call(t, http.MethodGet, oldPath, "", "", nil).Body.Bytes()...)
	if bytes.Contains(oldBytes, []byte("_has_more")) || first.Content.Reviews[0].CLIHasMore {
		t.Fatal("predecessor-shaped canonical snapshot required")
	}
	var oldCanonical string
	if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND revision=1`, f.project).Scan(&oldCanonical); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSnapshotContent(oldCanonical); err != nil {
		t.Fatal("legacy decoder refused", err)
	}
	hints, release, err := f.store.SubscribePublicProjection(f.project, "window-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	assertQuiet := func(want Snapshot) {
		t.Helper()
		before := publicationCounts(t, f.store)
		f.refresh(t)
		if !reflect.DeepEqual(f.public(t), want) || !reflectCounts(before, publicationCounts(t, f.store)) {
			t.Fatal("unchanged eligible window changed bytes/freshness/state")
		}
		select {
		case <-hints:
			t.Fatal("private-only/unchanged facts emitted public hint")
		default:
		}
	}
	assertQuiet(first)
	// The current source returns only its newest100; the stored published union
	// crosses101 without requiring any private observation above its old cap.
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", CLI: serviceWindowRounds(2, 100), Hosted: hosted, CLIHasMore: true}}, false)
	f.refresh(t)
	second := f.public(t)
	review := second.Content.Reviews[0]
	if second.Revision != 2 || second.Generation != first.Generation || len(review.CLI) != 100 || !review.CLIHasMore || review.HostedHasMore || !reflect.DeepEqual(review.Hosted, first.Content.Reviews[0].Hosted) || review.CLI[0].Key != cli[1].Key || review.CLI[99].Key != serviceWindowRounds(101, 1)[0].Key {
		t.Fatal("window did not advance independently and honestly")
	}
	select {
	case <-hints:
	default:
		t.Fatal("eligible window change did not emit hint")
	}
	var currentCanonical string
	if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND revision=2`, f.project).Scan(&currentCanonical); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSnapshotContent(currentCanonical); err != nil {
		t.Fatal("window canonical decoder refused", err)
	}
	if !bytes.Equal(oldBytes, f.call(t, http.MethodGet, oldPath, "", "", nil).Body.Bytes()) {
		t.Fatal("current window rewrote a frozen predecessor response")
	}
	assertQuiet(second)
	// Private omission does not withdraw previously approved summaries, flags,
	// merged lifecycle or channel facts and cannot manufacture fresh publication.
	f.setFacts(nil, false)
	assertQuiet(second)
	corrected := serviceWindowRounds(101, 1)
	duration := int64(1500)
	corrected[0].DurationMilliseconds = &duration
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", CLI: corrected}}, false)
	f.refresh(t)
	third := f.public(t)
	if third.Revision != 3 || len(third.Content.Reviews[0].CLI) != 100 || !third.Content.Reviews[0].CLIHasMore || third.Content.Reviews[0].CLI[99].DurationMilliseconds == nil || *third.Content.Reviews[0].CLI[99].DurationMilliseconds != duration {
		t.Fatal("same-key correction lost stable identity or known coverage")
	}
	if !bytes.Equal(oldBytes, f.call(t, http.MethodGet, oldPath, "", "", nil).Body.Bytes()) {
		t.Fatal("correction changed historical response")
	}
}

func TestPublicationWindowGenerationEnrollmentAndReplayStayExplicit(t *testing.T) {
	f := newPublicationFixture(t)
	f.setFacts([]ReviewFacts{{Number: 7, Lifecycle: "merged", Hosted: serviceWindowRounds(1, 101)}}, false)
	input := Approved{0, true, testPublicManifest()}
	preview := f.prepare(t, input)
	body := map[string]any{"expected_revision": input.ExpectedRevision, "enabled": true, "manifest": input.Manifest, "preview_sha256": preview.SHA256}
	applied := f.call(t, http.MethodPut, f.base()+"/publication", f.admin, "window-publish", body)
	if applied.Code != 200 {
		t.Fatal(applied.Code)
	}
	original := f.public(t)
	if !original.Content.Reviews[0].HostedHasMore || len(original.Content.Reviews[0].Hosted) != 100 {
		t.Fatal("preview/apply did not window")
	}
	before := publicationCounts(t, f.store)
	replay := f.call(t, http.MethodPut, f.base()+"/publication", f.admin, "window-publish", body)
	if replay.Code != 200 || replay.Body.String() != applied.Body.String() || !reflectCounts(before, publicationCounts(t, f.store)) {
		t.Fatal("exact publication replay changed state")
	}
	// Removing enrollment is an explicit withdrawal; reintroducing in a fresh
	// generation cannot resurrect prior rounds omitted from the current source.
	input.ExpectedRevision = 1
	input.Manifest.Reviews = []EnrolledReview{}
	if w := f.apply(t, input, "window-withdraw"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(f.public(t).Content.Reviews) != 0 {
		t.Fatal("withdrawn review persisted")
	}
	f.setFacts(nil, false)
	if w := f.call(t, http.MethodPost, f.base()+"/publication/unpublish", f.admin, "window-disable", map[string]any{"expected_revision": 2}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := f.call(t, http.MethodGet, fmt.Sprintf("/api/v1/public/projects/showcase/history/1?generation=%s", original.Generation), "", "", nil); w.Code != 404 {
		t.Fatal("unpublished history remained accessible")
	}
	input.ExpectedRevision = 3
	input.Manifest = testPublicManifest()
	if w := f.apply(t, input, "window-new-generation"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	current := f.public(t)
	if current.Generation == original.Generation || current.Revision != 1 || len(current.Content.Reviews[0].Hosted) != 0 || current.Content.Reviews[0].HostedHasMore {
		t.Fatal("fresh generation resurrected omitted public history")
	}
	var encoded map[string]any
	if json.Unmarshal(f.call(t, http.MethodGet, "/api/v1/public/projects/showcase", "", "", nil).Body.Bytes(), &encoded) != nil {
		t.Fatal("current JSON invalid")
	}
	if w := f.call(t, http.MethodGet, fmt.Sprintf("/api/v1/public/projects/showcase/history/1?generation=%s", original.Generation), "", "", nil); w.Code != http.StatusConflict {
		t.Fatal("old generation must require resynchronization", w.Code)
	}
}
