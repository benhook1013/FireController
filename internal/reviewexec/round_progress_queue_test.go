package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestQueuedReviewSlotDetailsSeparateHoldersAndPredecessors(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4, 6)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-project", "hosted", 4, 1))
	head := f.enqueue(t, attemptProject, "head-request", "hosted", 2, 1)
	prior := f.enqueue(t, attemptProject, "earlier-request", "hosted", 4, 1)
	f.enqueue(t, attemptProject, "queued-request", "hosted", 6, 1)

	read := func(version string) RoundProgressChannel {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=6&view=stages", nil)
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set(reviewQueueDetailsHeader, version)
		req.Header.Set(operationWaitHeader, "5")
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		var progress RoundProgress
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &progress) != nil || len(progress.Items) != 1 {
			t.Fatal(response.Code, response.Body.String())
		}
		if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), reviewQueueDetailsHeader) {
			t.Fatal("missing queue detail negotiation")
		}
		return progress.Items[0].Channels[0]
	}
	if legacy := read(""); legacy.Queue != nil || legacy.Wait != nil {
		t.Fatal("ordinary reader gained queue details", legacy)
	}
	queued := read("v1")
	if queued.Wait != nil || queued.Queue == nil || !slices.Equal(queued.Queue.Ahead, []int64{2, 4}) || len(queued.Queue.Holding) != 0 {
		t.Fatalf("ordinary queued request or project boundary was misread: %+v", queued.Queue)
	}
	f.start(t, attemptProject, head)
	queued = read("v1")
	if queued.Queue == nil || !slices.Equal(queued.Queue.Ahead, []int64{4}) || !slices.Equal(queued.Queue.Holding, []int64{2}) {
		t.Fatalf("reservation and waiting PRs were conflated: %+v", queued.Queue)
	}
	timerControl(t, f, prior.ID, "pause", "pause-predecessor")
	queued = read("v1")
	if queued.Queue == nil || len(queued.Queue.Ahead) != 0 || !slices.Equal(queued.Queue.Holding, []int64{2}) {
		t.Fatalf("paused predecessor still held its place: %+v", queued.Queue)
	}
}
