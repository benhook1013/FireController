package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestHostedDisplayStartSurvivesCompletion(t *testing.T) {
	f, attempt, input := hostedPublicationFixture(t, false)
	ctx := t.Context()
	now := time.Now().UTC()
	signal := ActivityEvidence{
		AttemptID: attempt.ID, HeadSHA: attempt.Anchor.CandidateSHA,
		Kind: "hosted_processing_observed", ObservedAt: now.Format(time.RFC3339Nano),
		CommentUpdatedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		SourceCommentID:  21, ProviderRunID: input.Hosted.ProviderAttemptID, TriggerID: attempt.ExternalID,
	}
	if err := f.e.RecordActivityEvidence(ctx, attemptProject, attempt.ID, signal); err != nil {
		t.Fatal(err)
	}
	input.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	receipt, err := f.e.RecordReviewEvidence(ctx, attemptProject, attempt.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.PublishEvidence(ctx, attemptProject, receipt.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, attempt)
	raw := cliPublicationDetail(t, f, 2, receipt.ID)
	if raw.Run.DisplayStartAt != "" || raw.Run.StartedAt != input.StartedAt || raw.Run.FinishedAt != input.FinishedAt {
		t.Fatal("ordinary response changed the recorded source times", raw.Run)
	}
	read := func(route string, dst any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews"+route, nil)
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set("X-FC-Review-Start", "v1")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), dst) != nil {
			t.Fatalf("read %s: %d %s", route, w.Code, w.Body.String())
		}
	}
	check := func(items []reviews.RunSummary) {
		t.Helper()
		for _, run := range items {
			if run.SourceRunID == receipt.ID {
				if run.DisplayStartAt != signal.ObservedAt || run.StartedAt != input.StartedAt || run.FinishedAt != input.FinishedAt {
					t.Fatal("display did not preserve the original activity and capture times", run)
				}
				return
			}
		}
		t.Fatal("missing published review")
	}
	for _, route := range []string{"/pulls/2/runs", "/pulls/2/runs?view=timeline"} {
		var response reviews.RunsResponse
		read(route, &response)
		check(response.Items)
	}
	for _, route := range []string{"/queue", "/queue?view=recent"} {
		var response reviews.QueueResponse
		read(route, &response)
		if len(response.Items) != 1 {
			t.Fatal("missing published PR", route, response.Items)
		}
		check(response.Items[0].RecentRuns)
	}
	var detail reviews.RunDetailResponse
	read("/runs/"+raw.Run.ID, &detail)
	check([]reviews.RunSummary{detail.Run})
	var pull reviews.PullResponse
	read("/pulls/2", &pull)
	check(pull.Pull.RecentRuns)
	// Another head cannot inherit the activity even if a retained diagnostic is corrupt.
	admissionSQL(t, f, `UPDATE review_activity_evidence SET body_json=json_set(body_json,'$.head_sha',?) WHERE attempt_id=?`, strings.Repeat("f", 40), attempt.ID)
	detail = reviews.RunDetailResponse{}
	read("/runs/"+raw.Run.ID, &detail)
	if detail.Run.DisplayStartAt != "" {
		t.Fatal("mismatched activity was displayed", detail.Run)
	}
}
