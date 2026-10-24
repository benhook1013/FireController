package client_test

import (
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"testing"
)

func TestReviewChannelCountsActualHTTPAndStrictShape(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t, func(req *domain.ObservationRequest) {
		run := req.Queue[0].Runs[0]
		req.Queue[0].Attempts = nil
		req.Queue[0].Runs = nil
		for i := 0; i < 24; i++ {
			v := run
			v.SourceRunID = fmt.Sprintf("channel-%d", i)
			v.Channel = []string{"hosted", "cli", "subagent", "manual"}[i%4]
			req.Queue[0].Runs = append(req.Queue[0].Runs, v)
		}
	})
	before := f.calls.Load()
	queue, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil || len(queue.Items) != 1 || queue.Items[0].RunCount != 24 || queue.Items[0].RunCounts != (client.ReviewChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) || f.calls.Load() != before+1 {
		t.Fatal("exact count must come with one queue read", queue, err)
	}
	pull, err := f.client.ReviewPull(t.Context(), 1, queue.SnapshotID)
	if err != nil || pull.Pull.RunCounts != queue.Items[0].RunCounts {
		t.Fatal(pull, err)
	}
	page, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{PageOptions: client.PageOptions{Limit: 20}, Snapshot: queue.SnapshotID})
	if err != nil || len(page.Items) != 20 || !page.HasMore {
		t.Fatal(page, err)
	}
	data, _ := json.Marshal(queue)
	valid := string(data)
	counts := `{"hosted":6,"cli":6,"subagent":6,"manual":6}`
	if !strings.Contains(valid, `"run_counts":`+counts) {
		t.Fatal("positive strict fixture lost counts")
	}
	good := newReviewMock(t, valid, "/api/v1/projects/project-one/reviews/queue")
	if _, err = good.client.ReviewQueue(t.Context(), client.PageOptions{}); err != nil {
		t.Fatal("positive fixture refused", err)
	}
	cases := []struct{ name, body string }{
		{"missing", strings.Replace(valid, `,"run_counts":`+counts, "", 1)},
		{"null", strings.Replace(valid, counts, `null`, 1)},
		{"missing-lane", strings.Replace(valid, counts, `{"hosted":6,"cli":6,"subagent":6}`, 1)},
		{"negative", strings.Replace(valid, counts, `{"hosted":-1,"cli":13,"subagent":6,"manual":6}`, 1)},
		{"mismatched-total", strings.Replace(valid, counts, `{"hosted":7,"cli":6,"subagent":6,"manual":6}`, 1)},
		{"fractional", strings.Replace(valid, counts, `{"hosted":6.5,"cli":6,"subagent":6,"manual":6}`, 1)},
		{"string", strings.Replace(valid, counts, `{"hosted":"6","cli":6,"subagent":6,"manual":6}`, 1)},
		{"unknown", strings.Replace(valid, counts, `{"hosted":6,"cli":6,"subagent":6,"manual":6,"other":0}`, 1)},
		{"duplicate", strings.Replace(valid, counts, `{"hosted":6,"cli":6,"subagent":6,"manual":6,"manual":6}`, 1)},
		{"overflow", strings.Replace(valid, counts, `{"hosted":9223372036854775807,"cli":9223372036854775807,"subagent":0,"manual":0}`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newReviewMock(t, tc.body, "/api/v1/projects/project-one/reviews/queue")
			_, err := mock.client.ReviewQueue(t.Context(), client.PageOptions{})
			checkError(t, err, "invalid_response")
		})
	}
}
