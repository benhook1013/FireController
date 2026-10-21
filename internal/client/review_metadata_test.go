package client_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewMetadataClientActualHTTPKnownZero(t *testing.T) {
	f := newReviewClientFixture(t)
	draft := false
	f.importSnapshot(t, func(req *domain.ObservationRequest) {
		req.Queue[0].Diff = &domain.PullDiff{}
		req.Queue[0].IsDraft = &draft
		req.Queue[0].MergedAt = "2026-01-01T00:00:00.123456789Z"
	})
	q, err := f.client.ReviewQueue(context.Background(), client.PageOptions{Limit: 5})
	if err != nil || len(q.Items) != 1 || q.Items[0].Diff == nil || q.Items[0].Diff.Files != 0 || q.Items[0].IsDraft == nil || *q.Items[0].IsDraft || q.Items[0].MergedAt != "2026-01-01T00:00:00.123456789Z" {
		t.Fatal("typed HTTP metadata lost", q, err)
	}
	p, err := f.client.ReviewPull(context.Background(), 1, q.SnapshotID)
	if err != nil || p.Pull.Diff == nil {
		t.Fatal(p, err)
	}
}

func TestReviewMetadataClientRefusesMalformedDisplayFacts(t *testing.T) {
	base := map[string]any{"project_id": "project-one", "revision": 1, "snapshot_id": "review_snapshot_" + strings.Repeat("b", 32), "source": "import", "observed_at": "2026-10-15T12:00:00Z", "recorded_at": "2026-10-15T12:01:00Z", "configured": true, "has_more": false}
	cases := []map[string]any{{"diff": map[string]any{}}, {"diff": map[string]any{"files": 0, "additions": 0}}, {"diff": map[string]any{"files": -1, "additions": 0, "deletions": 0}}, {"diff": map[string]any{"files": 2147483648, "additions": 0, "deletions": 0}}, {"is_draft": "false"}, {"merged_at": "invalid"}, {"state": "open", "merged_at": "2026-01-01T00:00:00Z"}}
	for _, change := range cases {
		p := map[string]any{"number": 1, "title": "PR", "state": "merged", "head_sha": "", "base_ref": "main", "base_sha": "", "ci_state": "unknown", "mergeability": "unknown", "complete": false, "run_count": 0, "run_counts": map[string]int{"hosted": 0, "cli": 0, "subagent": 0, "manual": 0}, "attempt_count": 0, "recent_runs": []any{}, "latest_attempts": []any{}}
		for key, value := range change {
			p[key] = value
		}
		base["items"] = []any{p}
		raw, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		f := newReviewMock(t, string(raw), "/api/v1/projects/project-one/reviews/queue")
		_, err = f.client.ReviewQueue(context.Background(), client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
}
