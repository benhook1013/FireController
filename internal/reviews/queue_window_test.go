package reviews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func windowRead(t *testing.T, f *apiFixture, query string) QueueResponse {
	t.Helper()
	w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/queue"+query, f.readToken, "", nil)
	var page QueueResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("queue window: %d %s", w.Code, w.Body.String())
	}
	return page
}

func windowNumbers(page QueueResponse) []int64 {
	result := make([]int64, len(page.Items))
	for i, item := range page.Items {
		result[i] = item.Number
	}
	return result
}

func windowToken(t *testing.T, f *apiFixture, cursor queueWindowCursor) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.readToken)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return f.store.Cursor(scope, "reviews.queue:"+cursor.View+":v1", string(raw))
}

func TestQueueWindowExactInstantsOrderingPositionsAndConservation(t *testing.T) {
	f := newAPIFixture(t)
	cutoff := time.Date(2026, 10, 18, 3, 0, 0, 123456789, time.UTC)
	merge := func(n int64, at time.Time) PullInput {
		p := samplePull(n, "window")
		p.MergedAt = at.Format(time.RFC3339Nano)
		return p
	}
	queue := []PullInput{
		merge(1, cutoff.Add(-10*24*time.Hour)),
		merge(2, cutoff.Add(-time.Nanosecond)),
		merge(3, cutoff.In(time.FixedZone("offset", 13*3600))),
		merge(4, cutoff.Add(24*time.Hour)),
		merge(5, cutoff.Add(-3*24*time.Hour)),
		samplePull(6, "unknown merge time"),
		merge(7, cutoff.Add(72*time.Hour)),
		samplePull(8, "closed"), samplePull(9, "open"), samplePull(10, "unknown"),
	}
	queue[7].State, queue[8].State, queue[9].State = "closed", "open", "unknown"
	f.observe(t, reviewRequest(0, queue), "window-fixture")
	all := windowRead(t, f, "")
	before := importDatabaseState(t, f)
	// A signed fixture cursor pins an exact nanosecond cutoff. Position zero
	// is an excluded old merge, so this observes the complete eligible slice.
	cursor := windowToken(t, f, queueWindowCursor{View: "recent", Snapshot: all.SnapshotID, Position: 0, Cutoff: cutoff.Format(time.RFC3339Nano)})
	page := windowRead(t, f, "?view=recent&limit=100&cursor="+url.QueryEscape(cursor))
	if got, want := windowNumbers(page), []int64{3, 4, 6, 7, 8, 9, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exact cutoff/offset/unknown/future filter: got%v want%v", got, want)
	}
	if page.HeadPosition == nil || *page.HeadPosition != 8 {
		t.Fatal("closed predecessors must not move conservative open head", page.HeadPosition)
	}
	for _, item := range page.Items {
		if item.QueuePosition == nil || *item.QueuePosition != int(item.Number)-1 {
			t.Fatal("filtered row lost original position", item.Number, item.QueuePosition)
		}
	}
	merged := windowRead(t, f, "?view=merged&limit=100")
	if got, want := windowNumbers(merged), []int64{7, 6, 5, 4, 3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history must reverse configured position, not merge time: %v", got)
	}
	if merged.HeadPosition == nil || *merged.HeadPosition != 8 {
		t.Fatal("history head must retain full queue position")
	}
	ordinary := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/queue?limit=100", f.readToken, "", nil)
	if strings.Contains(ordinary.Body.String(), "queue_position") || strings.Contains(ordinary.Body.String(), "head_position") || len(windowRead(t, f, "?limit=100").Items) != len(queue) {
		t.Fatal("ordinary all-queue shape or membership changed")
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("queue reads wrote retained rows, receipts or events")
	}
}

func TestQueueWindowNewestTwoStableTiesAndNoStarvation(t *testing.T) {
	f := newAPIFixture(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	queue := make([]PullInput, 30)
	for i := range queue {
		queue[i] = samplePull(int64(i+1), "old merge")
		queue[i].MergedAt = old.Format(time.RFC3339Nano)
	}
	queue[29].State, queue[29].MergedAt = "open", ""
	f.observe(t, reviewRequest(0, queue), "old-merged-prefix")
	page := windowRead(t, f, "?view=recent&limit=1")
	var got []int64
	for {
		got = append(got, windowNumbers(page)...)
		if !page.HasMore {
			break
		}
		page = windowRead(t, f, "?view=recent&limit=1&cursor="+url.QueryEscape(page.Cursor))
	}
	if !reflect.DeepEqual(got, []int64{1, 2, 30}) {
		t.Fatal("stable top-two known ties / hidden prefix starved open work", got)
	}
	page = windowRead(t, f, "?view=merged&limit=1")
	if !reflect.DeepEqual(windowNumbers(page), []int64{29}) || !page.HasMore {
		t.Fatal("merged descending first page", page)
	}
	page = windowRead(t, f, "?view=merged&limit=1&cursor="+url.QueryEscape(page.Cursor))
	if !reflect.DeepEqual(windowNumbers(page), []int64{28}) {
		t.Fatal("merged descending continuation", page)
	}
}

func TestQueueWindowCursorBindingCutoffAndRestart(t *testing.T) {
	f := newAPIFixture(t)
	first := samplePull(1, "old first")
	first.MergedAt = time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	second := first
	second.Number = 2
	open := samplePull(3, "original open")
	open.State = "open"
	f.observe(t, reviewRequest(0, []PullInput{first, second, open}), "pinned-window")
	page := windowRead(t, f, "?view=recent&limit=1")
	if page.Cursor == "" {
		t.Fatal("fixture requires continuation")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.readToken)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.store.ParseCursor(scope, "reviews.queue:recent:v1", page.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := queueWindowAfter("recent", after, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	later, err := queueWindowAfter("recent", after, time.Now().Add(20*24*time.Hour))
	if err != nil || later != initial {
		t.Fatal("cursor cutoff moved with wall clock", later, err)
	}
	base := "/api/v1/projects/" + f.project + "/reviews/queue"
	all := windowRead(t, f, "?limit=1")
	for _, query := range []string{
		"?view=all", "?view=", "?view=unknown", "?view=recent&view=recent",
		"?view=merged&cursor=" + url.QueryEscape(page.Cursor),
		"?cursor=" + url.QueryEscape(page.Cursor),
		"?view=recent&cursor=" + url.QueryEscape(all.Cursor),
		"?view=recent&cursor=x" + url.QueryEscape(page.Cursor),
	} {
		if w := f.call("GET", base+query, f.readToken, "", nil); w.Code != 400 {
			t.Errorf("invalid view/cursor accepted: %s status%d", query, w.Code)
		}
	}
	merged := windowRead(t, f, "?view=merged&limit=1")
	if w := f.call("GET", base+"?view=recent&cursor="+url.QueryEscape(merged.Cursor), f.readToken, "", nil); w.Code != 400 {
		t.Fatal("merged cursor accepted by recent view", w.Code)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('window-foreign','Foreign',702,'owner/foreign')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('review-read-credential','window-foreign',1,0)`); err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", "/api/v1/projects/window-foreign/reviews/queue?view=recent&cursor="+url.QueryEscape(page.Cursor), f.readToken, "", nil); w.Code != 400 {
		t.Fatal("foreign project cursor accepted", w.Code)
	}
	if w := f.call("GET", base+"?view=recent&cursor="+url.QueryEscape(page.Cursor), f.admin, "", nil); w.Code != 400 {
		t.Fatal("another principal's cursor accepted", w.Code)
	}
	open.Title = "new workspace"
	f.observe(t, reviewRequest(1, []PullInput{first, second, open}), "advanced-workspace")
	var seq int
	var name, dbPath string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	mux := http.NewServeMux()
	Register(mux, store)
	f.mux = mux
	continued := windowRead(t, f, "?view=recent&limit=100&cursor="+url.QueryEscape(page.Cursor))
	if continued.SnapshotID != page.SnapshotID || !reflect.DeepEqual(windowNumbers(continued), []int64{2, 3}) || continued.Items[1].Title != "original open" {
		t.Fatal("restart/new observation changed pinned selection", continued)
	}
}

func TestQueueWindowUnknownHeadAndHiddenCorruption(t *testing.T) {
	f := newAPIFixture(t)
	unknown, open := samplePull(1, "unknown"), samplePull(2, "open")
	unknown.State, open.State = "unknown", "open"
	merged := samplePull(3, "hidden malformed old history")
	merged.MergedAt = time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	f.observe(t, reviewRequest(0, []PullInput{unknown, open, merged}), "unknown-head")
	if page := windowRead(t, f, "?view=recent"); page.HeadPosition != nil {
		t.Fatal("unknown predecessor invented open head")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json='{"merged_at":false}' WHERE project_id=? AND number=3`, f.project); err != nil {
		t.Fatal(err)
	}
	before := importDatabaseState(t, f)
	for _, view := range []string{"recent", "merged"} {
		w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/queue?view="+view+"&limit=1", f.readToken, "", nil)
		if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"storage_unavailable"`) || strings.Contains(w.Body.String(), "merged_at") {
			t.Fatal("filtered hidden corruption failed open or leaked storage", w.Code, w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("failed queue filtering mutated storage")
	}
}

func TestQueueWindowNewestKnownUsesExactTimeRatherThanTimestampText(t *testing.T) {
	f := newAPIFixture(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	queue := make([]PullInput, 5)
	for index := range queue {
		queue[index] = samplePull(int64(index+1), "exact newest known")
	}
	queue[0].MergedAt = old.Format(time.RFC3339Nano)
	queue[1].MergedAt = old.Add(time.Nanosecond).In(time.FixedZone("positive", 13*3600)).Format(time.RFC3339Nano)
	queue[2].MergedAt = old.In(time.FixedZone("negative", -8*3600)).Format(time.RFC3339Nano)
	queue[3].MergedAt = old.Add(2 * time.Nanosecond).In(time.FixedZone("other", -3*3600)).Format(time.RFC3339Nano)
	f.observe(t, reviewRequest(0, queue), "exact-newest")
	page := windowRead(t, f, "?view=recent")
	if got, want := windowNumbers(page), []int64{2, 4, 5}; !reflect.DeepEqual(got, want) || page.HasMore || page.HeadPosition != nil {
		t.Fatal("newest known instants must compare exact chronology, leaving unknown merged visible", page)
	}
}

func TestQueueWindowEmptyAndZeroHead(t *testing.T) {
	f := newAPIFixture(t)
	for _, view := range []string{"recent", "merged"} {
		page := windowRead(t, f, "?view="+view)
		if page.Configured || page.Items == nil || len(page.Items) != 0 || page.HasMore || page.Cursor != "" || page.HeadPosition != nil {
			t.Fatal("unconfigured filtered queue changed empty semantics", page)
		}
	}
	open := samplePull(1, "open head at zero")
	open.State = "open"
	f.observe(t, reviewRequest(0, []PullInput{open}), "zero-head")
	page := windowRead(t, f, "?view=recent")
	if page.HeadPosition == nil || *page.HeadPosition != 0 || page.Items[0].QueuePosition == nil || *page.Items[0].QueuePosition != 0 {
		t.Fatal("zero is known placement, not missing", page)
	}
	page = windowRead(t, f, "?view=merged")
	if !page.Configured || len(page.Items) != 0 || page.HasMore || page.HeadPosition == nil || *page.HeadPosition != 0 {
		t.Fatal("empty merged view must preserve known full-queue head", page)
	}
}

func TestQueueWindowCutoffUsesFirstPageClock(t *testing.T) {
	now := time.Date(2026, 10, 20, 3, 0, 0, 123456789, time.FixedZone("offset", 13*3600))
	first, err := queueWindowAfter("recent", "", now)
	if err != nil || first.Position != -1 || first.Cutoff != now.UTC().Add(-48*time.Hour).Format(time.RFC3339Nano) {
		t.Fatal("first-page cutoff must be exactly 48 hours before its clock", first, err)
	}
	first.Snapshot, first.Position = "retained-snapshot", 0
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := queueWindowAfter("recent", string(raw), now.Add(30*24*time.Hour))
	if err != nil || continued != first {
		t.Fatal("continuation must retain first-page cutoff despite clock advance", continued, err)
	}
}

func TestQueueWindowLargeRetainedQueueAndPageOnlyDetails(t *testing.T) {
	f := newAPIFixture(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	// Ordinary bounded edits grow beyond a single request's 100-entry cap.
	// Unknown time precedes the known-time ties and cannot consume their slots.
	for batch := 0; batch < 3; batch++ {
		var pulls []QueuePullInput
		for number := batch*100 + 1; number <= (batch+1)*100 && number <= 203; number++ {
			pull := QueuePullInput{Number: int64(number), Title: "retained history", State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable", MergedAt: old}
			if number == 1 {
				pull.MergedAt = ""
			}
			if number == 203 {
				pull.State, pull.MergedAt = "open", ""
			}
			pulls = append(pulls, pull)
		}
		if result := queueEditHTTP(f, queueEditRequest(int64(batch), pulls, nil), fmt.Sprintf("window-growth-%d", batch)); result.Code != 201 {
			t.Fatal("bounded queue growth failed", result.Code, result.Body.String())
		}
	}
	// Poison a detail summary outside the eligible recent slice. Reading that
	// row explicitly still fails, while compact eligibility never decodes it.
	if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET recent_runs_json='invalid detailed summary' WHERE project_id=? AND number=100`, f.project); err != nil {
		t.Fatal(err)
	}
	before := importDatabaseState(t, f)
	page := windowRead(t, f, "?view=recent&limit=1")
	var got []int64
	for {
		if page.HeadPosition == nil || *page.HeadPosition != 202 || len(page.Items) != 1 {
			t.Fatal("large queue head or bounded page lost", page)
		}
		got = append(got, windowNumbers(page)...)
		if !page.HasMore {
			break
		}
		page = windowRead(t, f, "?view=recent&limit=1&cursor="+url.QueryEscape(page.Cursor))
	}
	if !reflect.DeepEqual(got, []int64{1, 2, 3, 203}) || page.Items[0].QueuePosition == nil || *page.Items[0].QueuePosition != 202 {
		t.Fatal("large retained queue was capped, ties shifted, or positions compressed", got, page)
	}
	merged := windowRead(t, f, "?view=merged&limit=1")
	if !reflect.DeepEqual(windowNumbers(merged), []int64{202}) || !merged.HasMore {
		t.Fatal("large merged history was capped or reordered", merged)
	}
	if w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/pulls/100", f.readToken, "", nil); w.Code != 500 {
		t.Fatal("poisoned detailed summary must fail when deliberately selected", w.Code)
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("large filtered reads changed retained storage")
	}
}

func TestQueueWindowDefaultAllMatchesFrozenReader(t *testing.T) {
	f := newAPIFixture(t)
	queue := []PullInput{samplePull(4, "retained merge"), samplePull(9, "open"), samplePull(2, "closed")}
	queue[1].State, queue[2].State = "open", "closed"
	f.observe(t, reviewRequest(0, queue), "all-compatibility")
	var cursor string
	for _, limit := range []int{1, 2} {
		query := fmt.Sprintf("?limit=%d", limit)
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		path := "/api/v1/projects/" + f.project + "/reviews/queue" + query
		request := httptest.NewRequest("GET", path, nil)
		request.SetPathValue("project_id", f.project)
		request.Header.Set("Authorization", "Bearer "+f.readToken)
		frozen := httptest.NewRecorder()
		// This predecessor reader is a separate frozen implementation of the
		// original all-queue ordering, signed cursor and response field set.
		metadataPre7Queue(f.store).ServeHTTP(frozen, request)
		current := f.call("GET", path, f.readToken, "", nil)
		if frozen.Code != 200 || current.Code != 200 || frozen.Body.String() != current.Body.String() {
			t.Fatalf("omitted view changed old wire bytes or cursor namespace: frozen=%d %s current=%d %s", frozen.Code, frozen.Body.String(), current.Code, current.Body.String())
		}
		var page QueueResponse
		if err := json.Unmarshal(current.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		cursor = page.Cursor
	}
}

func TestQueueWindowRetirementAndPinnedPlacement(t *testing.T) {
	f := newAPIFixture(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	queue := make([]PullInput, 4)
	for index := range queue {
		queue[index] = samplePull(int64(index+1), "retained placement")
		queue[index].MergedAt = old.Add(-time.Duration(index) * time.Hour).Format(time.RFC3339Nano)
	}
	queue[3].State, queue[3].MergedAt = "open", ""
	f.observe(t, reviewRequest(0, queue), "before-retirement")
	pinned := windowRead(t, f, "?view=recent&limit=1")
	if result := queueEditHTTP(f, queueEditRequest(1, nil, []RetiredPull{{Number: 1, Reason: "defunct"}}), "window-retirement"); result.Code != 201 {
		t.Fatal("explicit retirement failed", result.Code, result.Body.String())
	}
	current := windowRead(t, f, "?view=recent")
	if !reflect.DeepEqual(windowNumbers(current), []int64{2, 3, 4}) || current.HeadPosition == nil || *current.HeadPosition != 3 || current.Items[0].QueuePosition == nil || *current.Items[0].QueuePosition != 1 {
		t.Fatal("retired row consumed newest-two slot or compressed placement", current)
	}
	continued := windowRead(t, f, "?view=recent&cursor="+url.QueryEscape(pinned.Cursor))
	if continued.SnapshotID != pinned.SnapshotID || !reflect.DeepEqual(windowNumbers(continued), []int64{2, 4}) || continued.HeadPosition == nil || *continued.HeadPosition != 3 {
		t.Fatal("retirement changed pinned recent membership or placement", continued)
	}
	if got := windowNumbers(windowRead(t, f, "?view=merged")); !reflect.DeepEqual(got, []int64{3, 2}) {
		t.Fatal("retired row reappeared in current merged history", got)
	}
	if w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/pulls/1?snapshot="+pinned.SnapshotID, f.readToken, "", nil); w.Code != 200 {
		t.Fatal("display retirement removed retained detail", w.Code)
	}
}
