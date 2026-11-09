package client_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewQueueClientRetainsAndValidatesObserverHealth(t *testing.T) {
	const now = "2026-10-23T00:01:00Z"
	for _, kind := range []string{"valid", "bad-observer", "missing-start", "bad-state", "bad-time", "private-error", "missing-success", "missing-retry-error", "null-health"} {
		t.Run(kind, func(t *testing.T) {
			observer := map[string]any{"state": "running", "started_at": now}
			health := map[string]any{"state": "current", "checked_at": now, "last_success_at": now, "next_check_at": now, "ci_checked_at": now, "hosted_checked_at": now}
			switch kind {
			case "bad-observer":
				observer["state"] = "passing"
			case "missing-start":
				delete(observer, "started_at")
			case "bad-state":
				health["state"] = "ready"
			case "bad-time":
				health["ci_checked_at"] = "invalid"
			case "private-error":
				health["error"] = "PRIVATE provider response"
			case "missing-success":
				delete(health, "last_success_at")
			case "missing-retry-error":
				health["state"] = "retrying"
			}
			pull := map[string]any{"number": 1, "title": "PR", "state": "open", "head_sha": "", "base_ref": "main", "base_sha": "", "ci_state": "unknown", "mergeability": "unknown", "complete": false, "run_count": 0, "run_counts": map[string]int{"hosted": 0, "cli": 0, "subagent": 0, "manual": 0}, "attempt_count": 0, "recent_runs": []any{}, "latest_attempts": []any{}, "observation": health}
			if kind == "null-health" {
				pull["observation"] = nil
			}
			body := map[string]any{"project_id": "project-one", "revision": 1, "snapshot_id": "review_snapshot_" + strings.Repeat("b", 32), "source": "import", "observed_at": now, "recorded_at": now, "configured": true, "has_more": false, "observer": observer, "items": []any{pull}}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			f := newReviewMock(t, string(raw), "/api/v1/projects/project-one/reviews/queue")
			got, err := f.client.ReviewQueue(context.Background(), client.PageOptions{})
			if kind != "valid" {
				checkError(t, err, "invalid_response")
				return
			}
			if err != nil || got.Observer == nil || got.Observer.State != "running" || len(got.Items) != 1 || got.Items[0].Observation == nil || got.Items[0].Observation.LastSuccessAt != now || got.Items[0].Observation.CICheckedAt != now || got.Items[0].Observation.HostedCheckedAt != now {
				t.Fatal("observer health lost", got, err)
			}
		})
	}
}
