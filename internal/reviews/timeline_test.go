package reviews

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func timelineFixtureRun(id, channel, start, finish string) RunInput {
	return RunInput{SourceRunID: id, Channel: channel, Scope: "Retained original comparison", Outcome: "unknown", StartedAt: start, FinishedAt: finish, Findings: []Finding{}}
}
func TestTimelineChronologySnapshotAndScope(t *testing.T) {
	f := newAPIFixture(t)
	p := samplePull(12, "Timeline")
	p.Runs = []RunInput{
		timelineFixtureRun("whole", "hosted", "", "2026-10-01T00:00:00Z"),
		timelineFixtureRun("nano", "cli", "", "2026-10-01T00:00:00.000000001Z"),
		timelineFixtureRun("offset", "manual", "", "2026-10-01T01:00:00.5+01:00"),
		timelineFixtureRun("started", "subagent", "2026-10-01T00:00:01Z", ""),
		timelineFixtureRun("late-ingested-old", "hosted", "", "2026-09-01T00:00:00Z"),
		timelineFixtureRun("unknown", "hosted", "", ""),
		timelineFixtureRun("same-instant-later-order", "cli", "", "2026-10-01T00:00:00.000Z"),
	}
	f.observe(t, reviewRequest(0, []PullInput{p, samplePull(13, "Other")}), "timeline-first")
	before := historyViewsDigest(t, f)
	var first RunsResponse
	nativeRead(t, f, "/pulls/12/runs?view=timeline&limit=2", &first)
	if !first.HasMore || first.Cursor == "" {
		t.Fatal("missing continuation")
	}
	ids := []string{}
	for _, r := range first.Items {
		ids = append(ids, r.SourceRunID)
	}
	cursor := first.Cursor
	var oldSecond RunsResponse
	oldSecondBody := nativeRead(t, f, "/pulls/12/runs?view=timeline&limit=2&cursor="+url.QueryEscape(cursor), &oldSecond)
	for cursor != "" {
		var page RunsResponse
		nativeRead(t, f, "/pulls/12/runs?view=timeline&limit=2&cursor="+url.QueryEscape(cursor), &page)
		for _, r := range page.Items {
			ids = append(ids, r.SourceRunID)
		}
		cursor = page.Cursor
	}
	want := []string{"started", "offset", "nano", "same-instant-later-order", "whole", "late-ingested-old", "unknown"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatal(ids, want)
	}
	if before != historyViewsDigest(t, f) {
		t.Fatal("timeline read changed retained rows")
	}
	var normal RunsResponse
	body := nativeRead(t, f, "/pulls/12/runs?limit=1", &normal)
	if normal.Items[0].SourceRunID != "same-instant-later-order" {
		t.Fatal("default order changed")
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		t.Fatal("bad response")
	}
	if _, exists := raw["items"].([]any)[0].(map[string]any)["accepted_severity"]; exists {
		t.Fatal("default summaries gained timeline enrichment")
	}
	base := "/api/v1/projects/" + f.project + "/reviews/pulls/"
	for _, q := range []string{"view=", "view=other", "view=timeline&view=timeline", "cursor=" + url.QueryEscape(first.Cursor), "view=timeline&channel=cli&cursor=" + url.QueryEscape(first.Cursor)} {
		requireImportResponse(t, f.call("GET", base+"12/runs?"+q, f.readToken, "", nil), 400, "validation_failed")
	}
	requireImportResponse(t, f.call("GET", base+"13/runs?view=timeline&cursor="+url.QueryEscape(first.Cursor), f.readToken, "", nil), 400, "validation_failed")
	p.Runs = append(p.Runs, timelineFixtureRun("late-new", "hosted", "", "2026-10-02T00:00:00Z"))
	next := f.observe(t, reviewRequest(1, []PullInput{p, samplePull(13, "Other")}), "timeline-new")
	var pinned RunsResponse
	if got := nativeRead(t, f, "/pulls/12/runs?view=timeline&limit=2&cursor="+url.QueryEscape(first.Cursor), &pinned); string(got) != string(oldSecondBody) {
		t.Fatal("late ingestion changed old page")
	}
	requireImportResponse(t, f.call("GET", base+"12/runs?view=timeline&snapshot="+fmt.Sprint(next["id"])+"&cursor="+url.QueryEscape(first.Cursor), f.readToken, "", nil), 400, "validation_failed")
	var hosted RunsResponse
	nativeRead(t, f, "/pulls/12/runs?view=timeline&channel=hosted", &hosted)
	for _, r := range hosted.Items {
		if r.Channel != "hosted" {
			t.Fatal("channel leaked")
		}
	}
	if _, e := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='review-read-credential'`); e != nil {
		t.Fatal(e)
	}
	requireImportResponse(t, f.call("GET", base+"12/runs?view=timeline&cursor="+url.QueryEscape(first.Cursor), f.readToken, "", nil), 403, "forbidden")
}

func findingViewsForTest(findings []Finding) []FindingView {
	result := make([]FindingView, len(findings))
	for i, f := range findings {
		result[i] = FindingView{Finding: f}
	}
	return result
}
func plainFindingsForTest(findings []FindingView) []Finding {
	result := make([]Finding, len(findings))
	for i, f := range findings {
		result[i] = f.Finding
	}
	return result
}

func TestTimelineFinalizedInventoryUsesExactSourceIndex(t *testing.T) {
	f := newAPIFixture(t)
	rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT "+timelineFindingRevisionSQL+" FROM review_run_versions r WHERE r.project_id=? AND r.id=? AND r.version_revision=?", f.project, "review_run_example", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "review_run_versions_source_revision") || strings.Contains(joined, "TEMP B-TREE") || strings.Contains(joined, "SCAN v") {
		t.Fatal("original-finalization lookup lost its bounded index", joined)
	}
}

func TestTimelineAcceptedSeverityAndDetailPresentation(t *testing.T) {
	f := newAPIFixture(t)
	p := samplePull(14, "Severity timeline")
	finding := func(id, severity, detail, disposition string) Finding {
		return Finding{SourceFindingID: id, Title: "Original title " + id, Detail: detail, Severity: severity, Disposition: disposition}
	}
	run := func(id, channel string, accepted, found int64, complete bool, findings []Finding) RunInput {
		return RunInput{SourceRunID: id, Channel: channel, Scope: "Retained source", Outcome: "completed", Attributable: true, Complete: complete, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: found, Accepted: accepted, Findings: findings}
	}
	p.Runs = []RunInput{
		run("hosted", "hosted", 8, 9, true, []Finding{finding("critical", "critical", "original critical", "accepted"), finding("high", "high", "original high", "accepted"), finding("medium", "medium", "original medium", "accepted"), finding("low", "low", "original low", "accepted"), finding("info", "info", "original info", "accepted"), finding("display-major", "unknown", "_Major_", "accepted"), finding("display-minor", "unknown", "**Minor**", "accepted"), finding("unproven", "unknown", "Major word in ordinary prose.", "accepted"), finding("rejected", "high", "Excluded rejected detail", "rejected")}),
		run("cli-unknown", "cli", 1, 1, true, []Finding{finding("cli", "unknown", "**Critical**", "accepted")}),
		run("manual-unknown", "manual", 1, 1, true, []Finding{finding("manual", "unknown", "**Critical**", "accepted")}),
		run("frozen-mismatch", "hosted", 1, 2, true, []Finding{finding("a", "high", "A", "accepted"), finding("b", "low", "B", "accepted")}),
		run("missing-inventory", "hosted", 1, 2, false, []Finding{finding("partial", "high", "Partial retained source", "accepted")}),
		run("bounded-detail", "hosted", 1, 1, true, []Finding{finding("prefix", "unknown", strings.Repeat(" ", 4096)+"\n**Critical**", "accepted")}),
	}
	f.observe(t, reviewRequest(0, []PullInput{p}), "severity-import")
	before := historyViewsDigest(t, f)
	var response RunsResponse
	nativeRead(t, f, "/pulls/14/runs?view=timeline", &response)
	byID := map[string]RunSummary{}
	for _, r := range response.Items {
		byID[r.SourceRunID] = r
		if r.AcceptedSeverity == nil {
			t.Fatal("timeline breakdown missing")
		}
	}
	want := AcceptedSeverity{Critical: 1, Major: 2, Minor: 3, Trivial: 1, Unknown: 1, Complete: true}
	if got := byID["hosted"].AcceptedSeverity; *got != want {
		t.Fatal("accepted-only totals differ", *got, want)
	}
	for _, id := range []string{"cli-unknown", "manual-unknown", "bounded-detail"} {
		if got := byID[id].AcceptedSeverity; !got.Complete || got.Unknown != 1 || got.Critical != 0 {
			t.Fatal("unproven or non-Hosted body supplied credit", id, *got)
		}
	}
	for _, id := range []string{"frozen-mismatch", "missing-inventory"} {
		if got := byID[id].AcceptedSeverity; *got != (AcceptedSeverity{}) {
			t.Fatal("incomplete inventory fabricated buckets", id, *got)
		}
		if byID[id].Accepted != 1 {
			t.Fatal("frozen accepted changed")
		}
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+byID["hosted"].ID+"?view=display", &detail)
	for _, v := range detail.Findings {
		switch v.SourceFindingID {
		case "display-major":
			if v.Severity != "unknown" || v.Detail != "_Major_" || v.DisplaySeverity != "high" {
				t.Fatal("display changed raw source", v)
			}
		case "display-minor":
			if v.DisplaySeverity != "low" {
				t.Fatal("missing native display enum", v)
			}
		default:
			if v.DisplaySeverity != "" {
				t.Fatal("unexpected computed severity", v)
			}
		}
	}
	var cliDetail RunDetailResponse
	nativeRead(t, f, "/runs/"+byID["cli-unknown"].ID+"?view=display", &cliDetail)
	if cliDetail.Findings[0].DisplaySeverity != "" {
		t.Fatal("CLI body interpreted as Hosted")
	}
	var ordinary RunDetailResponse
	nativeRead(t, f, "/runs/"+byID["hosted"].ID, &ordinary)
	for _, finding := range ordinary.Findings {
		if finding.DisplaySeverity != "" {
			t.Fatal("ordinary client detail gained browser-only fields")
		}
	}
	for _, selector := range []string{"view=", "view=other", "view=display&view=display"} {
		requireImportResponse(t, f.call("GET", "/api/v1/projects/"+f.project+"/reviews/runs/"+byID["hosted"].ID+"?"+selector, f.readToken, "", nil), 400, "validation_failed")
	}
	if before != historyViewsDigest(t, f) {
		t.Fatal("read-side display enrichment changed retained facts")
	}
}

func TestTimelineSQLNanosecondsAndOffsets(t *testing.T) {
	f := newAPIFixture(t)
	for _, value := range []string{"2026-10-01T00:00:00Z", "2026-10-01T00:00:00.000000001Z", "2026-10-01T00:00:00.1Z", "2026-10-01T00:00:00.100Z", "2026-10-01T00:00:00.999999999Z", "2026-10-01T23:59:59.123456789-05:30", "0000-01-01T00:00:00+01:00", "9999-12-31T23:59:59.999999999-01:00"} {
		t.Run(value, func(t *testing.T) {
			parsed, e := time.Parse(time.RFC3339Nano, value)
			if e != nil {
				t.Fatal(e)
			}
			want := fmt.Sprintf("%020d.%09d", parsed.Unix()+62167305600, parsed.Nanosecond())
			var key string
			if e = f.store.DB().QueryRow(`WITH selected(timeline_time) AS (VALUES(?)) SELECT `+timelineKeySQL+` FROM selected`, value).Scan(&key); e != nil || key != want {
				t.Fatal(value, key, want, e)
			}
		})
	}
}
