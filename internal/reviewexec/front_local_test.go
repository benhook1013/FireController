package reviewexec

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestFrontsLocalPositionsIgnoreHiddenMergedHistory(t *testing.T) {
	f := frontFixture(t)
	one := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "local-three", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	old, recent, latest := projectionPull(2), projectionPull(9), projectionPull(10)
	for _, p := range []*reviews.PullInput{&old, &recent, &latest} {
		p.State = "merged"
	}
	old.MergedAt = time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	recent.MergedAt, latest.MergedAt = stamp(), stamp()
	var workspaceRevision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&workspaceRevision); err != nil {
		t.Fatal(err)
	}
	request := reviews.ObservationRequest{ExpectedRevision: &workspaceRevision, RepositoryID: 801, Source: "local-history-window", ObservedAt: stamp(), Queue: []reviews.PullInput{frontMergedPull(), old, projectionPull(4), projectionPull(6), recent, latest}}
	response := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "local-history-window", encoded(request))
	if response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
	frontClose(t, f, 4, "cli", "local-close-four-cli")
	window := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/queue?view=recent", f.reader, "", "")
	var queue reviews.QueueResponse
	if window.Code != http.StatusOK || json.Unmarshal(window.Body.Bytes(), &queue) != nil {
		t.Fatal(window.Code, window.Body.String())
	}
	for _, p := range queue.Items {
		if p.Number == 2 {
			t.Fatal("fixture failed to hide old merged predecessor")
		}
	}
	before := selectionCounts(t, f)
	value := frontRead(t, f)
	for channel, number := range map[string]int64{"hosted": 4, "cli": 6} {
		got := frontChannel(t, value, channel)
		if got.State != "held" || got.Reason != "local_queue_position" || got.PullNumber != number || got.SourceObservedAt != "" || !roundTime(got.CheckedAt) {
			t.Fatalf("local position claimed provider readiness or lost its front: %+v", got)
		}
	}
	if before != selectionCounts(t, f) {
		t.Fatal("local position read changed durable review state")
	}
}

func TestFrontsLocalPositionsStopAtMissingOrUnknownPredecessor(t *testing.T) {
	for _, state := range []string{"missing", "unknown", "retired"} {
		t.Run(state, func(t *testing.T) {
			f := frontFixture(t)
			if state == "missing" {
				f = frontSelectiveFixture(t)
			}
			switch state {
			case "missing":
				// This fixture deliberately has only PR 4 in its workspace.
			case "unknown":
				p := projectionPull(2)
				p.State = "unknown"
				one := int64(1)
				r := reviews.ObservationRequest{ExpectedRevision: &one, RepositoryID: 801, Source: "unknown-local-prefix", ObservedAt: stamp(), Queue: []reviews.PullInput{frontMergedPull(), p, projectionPull(4)}}
				w := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "unknown-local-prefix", encoded(r))
				if w.Code != http.StatusCreated {
					t.Fatal(w.Code, w.Body.String())
				}
			case "retired":
				var revision int64
				if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				r := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "retired-local-prefix", ObservedAt: stamp(), Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}, Queue: []reviews.PullInput{frontMergedPull(), projectionPull(4)}}
				w := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "retired-local-prefix", encoded(r))
				if w.Code != http.StatusCreated {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			got := frontChannel(t, frontRead(t, f), "cli")
			if got.State != "unknown" || got.PullNumber != 0 {
				t.Fatal("unknown or absent predecessor invented a later local position", got)
			}
		})
	}
}
