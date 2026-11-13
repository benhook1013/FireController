package client_test

import (
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewActiveQueueClientNativeAndStrictResponses(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t, func(r *domain.ObservationRequest) { r.Queue[0].State = "open" })
	out, err := f.client.ReviewActiveQueue(t.Context(), client.PageOptions{})
	if err != nil || out.ProjectID != f.project || out.SnapshotID != snapshot || out.Revision != 1 || !out.Configured || len(out.Items) != 1 || out.Items[0].Number != 1 || out.Items[0].Position != 0 || out.Items[0].RunCount != 1 {
		t.Fatal(out, err)
	}
	valid := client.ActiveQueueResponse{ProjectID: "project-one", Configured: true, SnapshotID: "review_snapshot_" + strings.Repeat("a", 32), Revision: 1, Items: []client.ActiveQueueItem{{Number: 1, Title: "Open pull", HeadSHA: strings.Repeat("b", 40), BaseRef: "main", CIState: "pending", Mergeability: "unknown", Position: 100, RunCount: 5}}}
	c, calls := progressResponseClient(t, progressJSON(t, valid))
	for _, o := range []client.PageOptions{{Limit: -1}, {Limit: 101}, {Cursor: "bad\nvalue"}} {
		if _, err := c.ReviewActiveQueue(t.Context(), o); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selection contacted service")
	}
	if _, err := c.ReviewActiveQueue(t.Context(), client.PageOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*client.ActiveQueueResponse){func(v *client.ActiveQueueResponse) { v.ProjectID = "other" }, func(v *client.ActiveQueueResponse) { v.Configured = false }, func(v *client.ActiveQueueResponse) { v.Revision = 0 }, func(v *client.ActiveQueueResponse) { v.SnapshotID = "bad" }, func(v *client.ActiveQueueResponse) { v.Items = nil }, func(v *client.ActiveQueueResponse) { v.HasMore = true }, func(v *client.ActiveQueueResponse) { v.Items[0].RunCount = -1 }, func(v *client.ActiveQueueResponse) { v.Items[0].Position = -1 }, func(v *client.ActiveQueueResponse) { v.Items[0].CIState = "ready" }, func(v *client.ActiveQueueResponse) { v.Items[0].HeadSHA = "bad" }, func(v *client.ActiveQueueResponse) { v.Items[0].Title = "" }, func(v *client.ActiveQueueResponse) { v.Items = append(v.Items, v.Items[0]) }, func(v *client.ActiveQueueResponse) {
		second := v.Items[0]
		second.Number = 2
		second.Position--
		v.Items = append(v.Items, second)
	}} {
		bad := valid
		bad.Items = append([]client.ActiveQueueItem(nil), valid.Items...)
		change(&bad)
		remote, _ := progressResponseClient(t, progressJSON(t, bad))
		if _, err := remote.ReviewActiveQueue(t.Context(), client.PageOptions{}); err == nil {
			t.Fatal("invalid compact response accepted", bad)
		}
	}
	body := progressJSON(t, valid)
	for _, bad := range []string{strings.Replace(body, `"position":100`, `"position":null`, 1), strings.Replace(body, `"position":100`, `"position":true`, 1), strings.Replace(body, `"position":100`, `"position":1.5`, 1), strings.Replace(body, `"position":100,`, "", 1), strings.Replace(body, `"run_count":5`, `"run_count":5,"state":"open"`, 1), strings.Replace(body, `"revision":1`, `"revision":1,"observed_at":"unused"`, 1), strings.Repeat(" ", 1<<20) + body} {
		remote, _ := progressResponseClient(t, bad)
		if _, err := remote.ReviewActiveQueue(t.Context(), client.PageOptions{}); err == nil {
			t.Fatal("malformed/extra/oversized active response accepted")
		}
	}
	remote, _ := progressResponseClient(t, progressJSON(t, client.ActiveQueueResponse{ProjectID: "project-one", Items: []client.ActiveQueueItem{}}))
	if _, err := remote.ReviewActiveQueue(t.Context(), client.PageOptions{}); err != nil {
		t.Fatal("valid unconfigured view refused", err)
	}
}
