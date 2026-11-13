package client_test

import (
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestJobStatusClientStrictCompactResponse(t *testing.T) {
	valid := client.JobStatus{ID: "job-one", Title: "Current task", WorkerID: "worker-one", Status: "blocked", Revision: 3, UpdatedAt: "2026-10-24T12:00:00Z", ProgressSummary: strings.Repeat("雪", 341), Next: "continue", Blocker: "dependency", ProgressTruncated: true, EvidenceCount: 2}
	c, calls := progressResponseClient(t, progressJSON(t, valid))
	if _, err := c.JobStatus(t.Context(), "../other"); err == nil || calls.Load() != 0 {
		t.Fatal("invalid job contacted service")
	}
	got, err := c.JobStatus(t.Context(), "job-one")
	if err != nil || got != valid || calls.Load() != 1 {
		t.Fatal(got, err, calls.Load())
	}
	for _, change := range []func(*client.JobStatus){func(v *client.JobStatus) { v.ID = "other" }, func(v *client.JobStatus) { v.Revision = 0 }, func(v *client.JobStatus) { v.WorkerID = "bad/id" }, func(v *client.JobStatus) { v.Status = "open" }, func(v *client.JobStatus) { v.Title = "" }, func(v *client.JobStatus) { v.UpdatedAt = "yesterday" }, func(v *client.JobStatus) { v.ProgressSummary = strings.Repeat("x", 1025) }, func(v *client.JobStatus) { v.Next = strings.Repeat("x", 513) }, func(v *client.JobStatus) { v.Blocker = strings.Repeat("x", 513) }, func(v *client.JobStatus) { v.EvidenceCount = 11 }, func(v *client.JobStatus) { v.EvidenceCount = -1 }, func(v *client.JobStatus) { v.ProgressSummary = "short" }, func(v *client.JobStatus) { v.NextTruncated = true }} {
		bad := valid
		change(&bad)
		c, _ := progressResponseClient(t, progressJSON(t, bad))
		if _, err := c.JobStatus(t.Context(), "job-one"); err == nil {
			t.Fatal("accepted invalid compact status", bad)
		}
	}
	body := progressJSON(t, valid)
	for _, bad := range []string{strings.Replace(body, `"next_truncated":false,`, "", 1), strings.Replace(body, `"next_truncated":false`, `"next_truncated":null`, 1), strings.Replace(body, `"evidence_count":2`, `"evidence_count":2.5`, 1), strings.Replace(body, `"id":"job-one"`, `"id":"job-one","brief":"private"`, 1), strings.Replace(body, `"id":"job-one"`, `"id":"job-one","id":"job-one"`, 1), strings.Repeat(" ", 16<<10) + body} {
		c, _ := progressResponseClient(t, bad)
		if _, err := c.JobStatus(t.Context(), "job-one"); err == nil {
			t.Fatal("accepted malformed/oversized response")
		}
	}
}
