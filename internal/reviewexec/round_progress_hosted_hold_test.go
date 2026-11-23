package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestRoundProgressHostedLaneHold(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	start := func(project string, number int64) Attempt {
		t.Helper()
		a := f.start(t, project, f.enqueue(t, project, "hosted-lane-holder", "hosted", number, 1))
		a, err := f.e.Admit(t.Context(), project, a.ID, a.Revision, attemptAnchor(project, number), "fixture-intent")
		if err != nil {
			t.Fatal(err)
		}
		a, err = f.e.Advance(t.Context(), project, a.ID, a.Revision, "active", "trigger_recorded", "42")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	foreign := start(otherAttemptProject, 4)
	holder := start(attemptProject, 2)
	f.enqueue(t, attemptProject, "waiting-hosted", "hosted", 4, 1)
	read := func(project, token, query, details string) RoundProgress {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, roundProgressPath(project)+"?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(reviewQueueDetailsHeader, details)
		req.Header.Set(operationWaitHeader, "5")
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		var progress RoundProgress
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &progress) != nil || progress.ProjectID != project {
			t.Fatal(response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), `"hosted_lane_hold"`) != (progress.HostedLaneHold != nil) ||
			!strings.Contains(strings.Join(response.Header().Values("Vary"), ","), reviewQueueDetailsHeader) {
			t.Fatal("invalid hold wire presence or negotiation", response.Header(), response.Body.String())
		}
		return progress
	}
	const staged = "pull_numbers=4&view=stages"
	if result := read(attemptProject, f.reader, staged, "v2"); result.HostedLaneHold == nil || result.HostedLaneHold.Attempt.State != "active" || result.HostedLaneHold.Attempt.Reason != "" {
		t.Fatal("active reservation not projected", result)
	}
	var err error
	const reason = "hosted_finished_inventory_unproven"
	holder, err = f.e.Advance(t.Context(), attemptProject, holder.ID, holder.Revision, "uncertain", reason, holder.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	moved := projectionPull(2)
	moved.HeadSHA = strings.Repeat("9", 40)
	newer := reviews.Attempt{SourceAttemptID: "newer-hosted-observation", Channel: "hosted", HeadSHA: moved.HeadSHA, State: "prepared", StartedAt: stamp()}
	projectionPublish(t, f, reviews.NativePublication{Pull: moved, Attempt: &newer})
	var pull reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &pull)
	if pull.Pull.HeadSHA != moved.HeadSHA || len(pull.Pull.LatestAttempts) != 1 || pull.Pull.LatestAttempts[0].SourceAttemptID != newer.SourceAttemptID {
		t.Fatal("fixture did not supersede latest display without releasing the old holder", pull)
	}
	before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, holder)
	for _, query := range []string{staged, "pull_numbers=2,4&view=stages"} {
		result := read(attemptProject, f.reader, query, "v2")
		hold := result.HostedLaneHold
		if hold == nil || hold.PullNumber != 2 || hold.Attempt.SourceAttemptID != holder.ID || hold.Attempt.HeadSHA != holder.Anchor.CandidateSHA ||
			hold.Attempt.Channel != "hosted" || hold.Attempt.State != "uncertain" || hold.Attempt.TriggerID != "42" || hold.Attempt.Reason != reason ||
			hold.Attempt.FinishedAt != "" || hold.Attempt.DurationSeconds != nil || hold.Attempt.SourceRunID != "" || hold.Attempt.ProviderReviewID != "" {
			t.Fatal("missing or incorrect original reservation", hold)
		}
		if _, err := time.Parse(time.RFC3339Nano, hold.Attempt.StartedAt); err != nil {
			t.Fatal("missing original journal time", hold, err)
		}
		waiting := result.Items[len(result.Items)-1].Channels[0]
		if waiting.Queue == nil || !slices.Equal(waiting.Queue.Holding, []int64{2}) || waiting.Used != 0 {
			t.Fatal("v2 changed queued holder details or completion credit", waiting)
		}
	}
	legacy := read(attemptProject, f.reader, staged, "v1")
	if legacy.HostedLaneHold != nil || legacy.Items[0].Channels[0].Queue == nil {
		t.Fatal("v1 response shape changed", legacy)
	}
	if read(attemptProject, f.reader, staged, "").HostedLaneHold != nil || read(attemptProject, f.reader, "pull_numbers=4", "v2").HostedLaneHold != nil {
		t.Fatal("hold leaked outside staged v2")
	}
	if after := roundTables(t, f) + policyState(t, f) + attemptState(t, f, holder); after != before {
		t.Fatal("progress read changed controls, journal, receipts or events")
	}
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=json_set(anchor_json,'$.candidate_sha',?) WHERE id=?`, moved.HeadSHA, holder.ID)
	if result := read(attemptProject, f.reader, staged, "v2"); result.HostedLaneHold != nil {
		t.Fatal("contradictory admission and operation heads produced a holder", result.HostedLaneHold)
	}
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=json_set(anchor_json,'$.candidate_sha',?) WHERE id=?`, holder.Anchor.CandidateSHA, holder.ID)
	admissionSQL(t, f, `UPDATE review_execution_reservations SET execution_ended_at=? WHERE attempt_id=?`, stamp(), holder.ID)
	if result := read(attemptProject, f.reader, staged, "v2"); result.HostedLaneHold != nil {
		t.Fatal("ended reservation produced a holder", result.HostedLaneHold)
	}
	admissionSQL(t, f, `UPDATE review_execution_reservations SET execution_ended_at='' WHERE attempt_id=?`, holder.ID)

	// Terminal execution releases this reservation. The other project's holder
	// uses the same provider account and PR number as the selected waiting PR.
	if _, err := f.e.Advance(t.Context(), attemptProject, holder.ID, holder.Revision, "failed", "fixture_release", holder.ExternalID); err != nil {
		t.Fatal(err)
	}
	if result := read(attemptProject, f.reader, staged, "v2"); result.HostedLaneHold != nil {
		t.Fatal("released or foreign reservation leaked into staged progress", result.HostedLaneHold)
	}
	if result := read(otherAttemptProject, f.other, staged, "v2"); result.HostedLaneHold == nil || result.HostedLaneHold.Attempt.SourceAttemptID != foreign.ID {
		t.Fatal("foreign reservation was not visible to its own project", result.HostedLaneHold)
	}
}
