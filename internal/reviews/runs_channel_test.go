package reviews

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
)

func channelRunsFixture(t *testing.T) (*apiFixture, PullInput, string) {
	t.Helper()
	f := newAPIFixture(t)
	p := samplePull(2967, "Skewed retained review channels")
	channels := []string{}
	for _, group := range []struct {
		channel string
		count   int
	}{{"hosted", 8}, {"cli", 17}, {"subagent", 2}, {"cli", 14}, {"subagent", 6}} {
		for i := 0; i < group.count; i++ {
			channels = append(channels, group.channel)
		}
	}
	for i, channel := range channels {
		p.Runs = append(p.Runs, RunInput{SourceRunID: fmt.Sprintf("retained-%02d", i), Channel: channel, Scope: "full", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-10-01T00:00:00Z", FinishedAt: "2026-10-01T00:01:00Z", Findings: []Finding{}})
	}
	receipt := f.observe(t, reviewRequest(0, []PullInput{p, samplePull(2968, "Unrelated PR")}), "channel-seed")
	return f, p, receipt["id"].(string)
}

func TestRunsChannelFiltersBeforeLimitAndPinsContinuation(t *testing.T) {
	f, p, snapshot := channelRunsFixture(t)
	var global RunsResponse
	nativeRead(t, f, "/pulls/2967/runs", &global)
	if len(global.Items) != 20 || !global.HasMore {
		t.Fatal("global default page changed")
	}
	counts := map[string]int{}
	for _, run := range global.Items {
		counts[run.Channel]++
	}
	if counts["cli"] != 14 || counts["subagent"] != 6 || counts["hosted"] != 0 {
		t.Fatal("fixture did not reproduce hidden Hosted channel", counts)
	}
	// Keep the exact legacy no-channel cursor filter and continuation identity.
	request := httptest.NewRequest("GET", "/", nil)
	request.SetPathValue("project_id", f.project)
	request.Header.Set("Authorization", "Bearer "+f.readToken)
	scope, err := f.store.Scope(request, "read")
	if err != nil {
		t.Fatal(err)
	}
	if global.Cursor != f.store.Cursor(scope, "reviews.runs:2967:v1", encodePosition(snapshot, 27)) {
		t.Fatal("legacy unfiltered cursor identity changed")
	}
	var allHosted, first RunsResponse
	nativeRead(t, f, "/pulls/2967/runs?channel=hosted", &allHosted)
	if len(allHosted.Items) != 8 || allHosted.HasMore || allHosted.Cursor != "" {
		t.Fatal("channel limit was applied after the global page", allHosted)
	}
	for i, run := range allHosted.Items {
		if run.Channel != "hosted" || run.SourceRunID != fmt.Sprintf("retained-%02d", 7-i) {
			t.Fatal("selected channel is not newest-first", run)
		}
	}
	nativeRead(t, f, "/pulls/2967/runs?channel=hosted&limit=3", &first)
	if len(first.Items) != 3 || !first.HasMore || first.Cursor == "" {
		t.Fatal("selected page did not preserve bound")
	}
	var second RunsResponse
	route := "/pulls/2967/runs?channel=hosted&limit=3&cursor=" + url.QueryEscape(first.Cursor)
	oldSecond := nativeRead(t, f, route, &second)
	if len(second.Items) != 3 || !second.HasMore || second.Items[0].SourceRunID != "retained-04" {
		t.Fatal("channel continuation skipped or duplicated runs", second)
	}
	var last RunsResponse
	nativeRead(t, f, "/pulls/2967/runs?channel=hosted&limit=3&cursor="+url.QueryEscape(second.Cursor), &last)
	if len(last.Items) != 2 || last.HasMore || last.Cursor != "" || last.Items[1].SourceRunID != "retained-00" {
		t.Fatal("selected final page not bounded", last)
	}
	next := p.Runs[0]
	next.SourceRunID = "later-hosted"
	p.Runs = append(p.Runs, next)
	updated := f.observe(t, reviewRequest(1, []PullInput{p, samplePull(2968, "Unrelated PR")}), "channel-newer")
	var pinned RunsResponse
	if got := nativeRead(t, f, route, &pinned); string(got) != string(oldSecond) || pinned.SnapshotID != snapshot {
		t.Fatal("new observation changed selected historical continuation")
	}
	var current RunsResponse
	nativeRead(t, f, "/pulls/2967/runs?channel=hosted&limit=1", &current)
	if current.SnapshotID != updated["id"] || current.Items[0].SourceRunID != "later-hosted" {
		t.Fatal("unselected snapshot did not read current channel")
	}
	for channel, expected := range map[string]int{"cli": 31, "subagent": 8} {
		var page RunsResponse
		nativeRead(t, f, "/pulls/2967/runs?channel="+channel+"&limit=100", &page)
		if len(page.Items) != expected || page.HasMore {
			t.Fatal("selected retained channel total changed", channel, len(page.Items))
		}
		for _, run := range page.Items {
			if run.Channel != channel {
				t.Fatal("selected channel leaked another channel", channel)
			}
		}
	}
	var empty RunsResponse
	nativeRead(t, f, "/pulls/2967/runs?channel=manual", &empty)
	if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.Cursor != "" {
		t.Fatal("empty selected channel changed DTO semantics")
	}
	before := historyViewsDigest(t, f)
	nativeRead(t, f, "/pulls/2967/runs?channel=cli&limit=100", new(RunsResponse))
	if before != historyViewsDigest(t, f) {
		t.Fatal("filtered read wrote durable state")
	}
}

func TestRunsChannelStrictQueryCursorScopeAndCurrentAuthority(t *testing.T) {
	f, _, snapshot := channelRunsFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	var selected, global RunsResponse
	nativeRead(t, f, "/pulls/2967/runs?channel=hosted&limit=2", &selected)
	nativeRead(t, f, "/pulls/2967/runs?limit=2", &global)
	for name, query := range map[string]string{
		"blank": "channel=", "missing value": "channel", "uppercase": "channel=Hosted", "unsupported": "channel=other", "space": "channel=%20hosted", "repeated": "channel=hosted&channel=hosted", "mixed repeated": "channel=hosted&channel=cli", "malformed encoding": "channel=%ZZ", "limit too large": "channel=hosted&limit=101", "unknown field": "channel=hosted&channels=cli",
		"switch channel": "channel=cli&cursor=" + url.QueryEscape(selected.Cursor), "omit channel": "cursor=" + url.QueryEscape(selected.Cursor), "unfiltered cursor into channel": "channel=hosted&cursor=" + url.QueryEscape(global.Cursor),
	} {
		t.Run(name, func(t *testing.T) {
			requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?"+query, f.readToken, "", nil), 400, "validation_failed")
		})
	}
	requireImportResponse(t, f.call("GET", base+"/pulls/2968/runs?channel=hosted&cursor="+url.QueryEscape(selected.Cursor), f.readToken, "", nil), 400, "validation_failed")
	requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?channel=hosted&cursor="+url.QueryEscape(selected.Cursor), f.admin, "", nil), 400, "validation_failed")
	requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?channel=hosted", "", "", nil), 401, "unauthorized")
	requireImportResponse(t, f.call("GET", base+"/pulls/999/runs?channel=hosted&snapshot="+snapshot, f.readToken, "", nil), 404, "not_found")
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('foreign-project','Foreign',702,'owner/foreign'); INSERT INTO grants VALUES('review-read-credential','foreign-project',1,0)`); err != nil {
		t.Fatal(err)
	}
	requireImportResponse(t, f.call("GET", "/api/v1/projects/foreign-project/reviews/pulls/2967/runs?channel=hosted&cursor="+url.QueryEscape(selected.Cursor), f.readToken, "", nil), 400, "validation_failed")
	// A real newer snapshot cannot be paired with the old channel cursor.
	receipt := f.observe(t, reviewRequest(1, []PullInput{samplePull(2967, "New view"), samplePull(2968, "Unrelated PR")}), "channel-replace")
	requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?channel=hosted&snapshot="+receipt["id"].(string)+"&cursor="+url.QueryEscape(selected.Cursor), f.readToken, "", nil), 400, "validation_failed")
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='review-read-credential'`); err != nil {
		t.Fatal(err)
	}
	requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?channel=hosted&cursor="+url.QueryEscape(selected.Cursor), f.readToken, "", nil), 403, "forbidden")
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='review-read-credential'`); err != nil {
		t.Fatal(err)
	}
	requireImportResponse(t, f.call("GET", base+"/pulls/2967/runs?channel=hosted&cursor="+url.QueryEscape(selected.Cursor), f.readToken, "", nil), 401, "unauthorized")
	// Response shape remains the existing RunsResponse; no filter field is added.
	encoded, _ := json.Marshal(selected)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	if _, found := fields["channel"]; found {
		t.Fatal("filtered endpoint changed response DTO")
	}
}
