package publicsite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

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
