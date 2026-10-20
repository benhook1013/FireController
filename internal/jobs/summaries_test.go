package jobs_test

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/jobs"
)

func summaryBody(job, revision int64, title, body string) map[string]any {
	return map[string]any{"expected_job_revision": job, "expected_revision": revision, "title": title, "body": body}
}
func summaryRead(t *testing.T, a *testAPI, base, token string) jobs.Summary {
	t.Helper()
	r := a.call("GET", base+"/summary", token, "", nil)
	progressOK(t, r, 200)
	var s jobs.Summary
	decodeResponse(t, r, &s)
	return s
}
func TestSummaryIndependentHistoryReplayAndDelivery(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	lookups := a.lookups
	beforeJob := a.call("GET", base, token, "", nil).Body.String()
	beforeHistory := a.call("GET", base+"/history", token, "", nil).Body.String()
	initial := summaryRead(t, a, base, token)
	if initial.Revision != 0 || initial.JobRevision != 1 || initial.Title != "" || initial.Body != "" {
		t.Fatal(initial)
	}
	initialNoop := a.callWorker("PUT", base+"/summary", token, worker, "empty", summaryBody(1, 0, "", ""))
	progressOK(t, initialNoop, 200)
	request := summaryBody(1, 0, "Deliberate public title 雪", "A concise **Markdown** account.\nNo private excerpt.")
	first := a.callWorker("PUT", base+"/summary", token, worker, "set-1", request)
	progressOK(t, first, 200)
	if strings.Contains(first.Body.String(), "Markdown") || !strings.Contains(first.Body.String(), `"revision":1`) {
		t.Fatal(first.Body.String())
	}
	current := summaryRead(t, a, base, token)
	if current.Revision != 1 || current.Title != request["title"] || current.Body != request["body"] {
		t.Fatal(current)
	}
	if got := a.call("GET", base, token, "", nil).Body.String(); got != beforeJob {
		t.Fatal("summary changed private job")
	}
	if got := a.call("GET", base+"/history", token, "", nil).Body.String(); got != beforeHistory {
		t.Fatal("summary changed private history")
	}
	delivery := a.call("GET", "/api/v1/projects/"+p+"/delivery", token, "", nil)
	progressOK(t, delivery, 200)
	var cards struct {
		Jobs struct {
			Items []struct {
				Summary *jobs.Summary `json:"summary"`
			} `json:"items"`
		} `json:"jobs"`
	}
	decodeResponse(t, delivery, &cards)
	if len(cards.Jobs.Items) != 1 || cards.Jobs.Items[0].Summary == nil || !reflect.DeepEqual(*cards.Jobs.Items[0].Summary, current) {
		t.Fatal("delivery missing exact authored summary")
	}
	progressOK(t, a.call("PATCH", base, token, "private-edit", map[string]any{"expected_revision": 1, "title": "Changed private title", "brief": "Changed private secret"}), 200)
	afterPrivate := summaryRead(t, a, base, token)
	if afterPrivate.Revision != 1 || afterPrivate.JobRevision != 2 || afterPrivate.Title != current.Title || afterPrivate.Body != current.Body {
		t.Fatal(afterPrivate)
	}
	progressOK(t, a.callWorker("PUT", base+"/summary", token, worker, "stale-job", summaryBody(1, 1, "Wrong", "Wrong")), 409)
	progressOK(t, a.callWorker("PUT", base+"/summary", token, worker, "stale-summary", summaryBody(2, 0, "Wrong", "Wrong")), 409)
	progressOK(t, a.callWorker("PUT", base+"/summary", token, worker, "clear", summaryBody(2, 1, "", "")), 200)
	cleared := summaryRead(t, a, base, token)
	if cleared.Revision != 2 || cleared.Title != "" || cleared.Body != "" {
		t.Fatal(cleared)
	}
	replay := a.callWorker("PUT", base+"/summary", token, worker, "set-1", request)
	progressOK(t, replay, 200)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("original key did not replay after private edit and clear")
	}
	if summaryRead(t, a, base, token).Revision != 2 {
		t.Fatal("replay replaced cleared state")
	}
	retained := a.call("GET", base+"/summary/history/1", token, "", nil)
	progressOK(t, retained, 200)
	var rev jobs.SummaryRevision
	decodeResponse(t, retained, &rev)
	if rev.Summary != current || rev.ActorID == "" || rev.WorkerID != worker || rev.CreatedAt == "" {
		t.Fatal(rev)
	}
	var page struct {
		Items   []jobs.SummaryRevision `json:"items"`
		Cursor  string                 `json:"cursor"`
		HasMore bool                   `json:"has_more"`
	}
	r := a.call("GET", base+"/summary/history?limit=1", token, "", nil)
	progressOK(t, r, 200)
	decodeResponse(t, r, &page)
	if !page.HasMore || len(page.Items) != 1 || page.Items[0].Revision != 2 {
		t.Fatal(page)
	}
	r = a.call("GET", base+"/summary/history?limit=1&cursor="+page.Cursor, token, "", nil)
	progressOK(t, r, 200)
	decodeResponse(t, r, &page)
	if page.HasMore || len(page.Items) != 1 || page.Items[0].Revision != 1 {
		t.Fatal(page)
	}
	progressOK(t, a.call("GET", base+"/summary/history/3", token, "", nil), 404)
	if a.lookups != lookups {
		t.Fatal("summary contacted repository provider")
	}
}

func TestSummaryAuthorizationValidationCASAndRollback(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	reader := a.grant(p, "summary-read", "read")
	other := a.createProject("other", "example/summary-other")
	otherToken := a.grant(other, "summary-other", "read", "write")
	progressOK(t, a.call("GET", base+"/summary", "", "", nil), 401)
	progressOK(t, a.call("PUT", base+"/summary", reader, "read-write", summaryBody(1, 0, "Title", "Body")), 403)
	progressOK(t, a.call("GET", base+"/summary", otherToken, "", nil), 403)
	for _, suffix := range []string{"/summary", "/summary/history", "/summary/history/1"} {
		progressOK(t, a.call("GET", strings.Replace(base, p, other, 1)+suffix, otherToken, "", nil), 404)
	}
	for name, body := range map[string]map[string]any{
		"one empty": summaryBody(1, 0, "", "Body"), "blank": summaryBody(1, 0, "  ", "Body"), "body bytes": summaryBody(1, 0, "Title", strings.Repeat("雪", 667)), "title bytes": summaryBody(1, 0, strings.Repeat("雪", 81), "Body"), "title newline": summaryBody(1, 0, "Title\nBad", "Body"), "body control": summaryBody(1, 0, "Title", "Body\x00"), "missing body": {"expected_job_revision": 1, "expected_revision": 0, "title": "Title"}, "null": {"expected_job_revision": 1, "expected_revision": 0, "title": nil, "body": nil}, "bool revision": {"expected_job_revision": true, "expected_revision": 0, "title": "Title", "body": "Body"}, "float revision": {"expected_job_revision": 1, "expected_revision": 0.1, "title": "Title", "body": "Body"}, "overflow": summaryBody(1, 9223372036854775807, "Title", "Body"),
	} {
		t.Run(name, func(t *testing.T) { progressOK(t, a.call("PUT", base+"/summary", token, "invalid-"+name, body), 400) })
	}
	progressOK(t, a.call("PUT", base+"/summary", token, "", summaryBody(1, 0, "Title", "Body")), 400)
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, key := range []string{"winner-a", "winner-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			results <- a.callWorker("PUT", base+"/summary", token, worker, key, summaryBody(1, 0, key, "Body"))
		}(key)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for r := range results {
		counts[r.Code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
	current := summaryRead(t, a, base, token)
	var history, events, receipts int
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM job_summary_history`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_summary_history BEFORE INSERT ON job_summary_history BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.callWorker("PUT", base+"/summary", token, worker, "rolled-back", summaryBody(1, 1, "Should roll back", "Body")), 500)
	if got := summaryRead(t, a, base, token); got != current {
		t.Fatal("history failure changed current summary")
	}
	for table, want := range map[string]int{"job_summary_history": history, "events": events, "receipts": receipts} {
		var got int
		if err := a.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatal(table, got, want, err)
		}
	}
}

func TestSummarySchemaUpgradeConservesPrivateRows(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	before := a.call("GET", base, token, "", nil).Body.String()
	history := a.call("GET", base+"/history", token, "", nil).Body.String()
	if _, err := a.store.DB().Exec(`DROP TABLE job_summary_history;DROP TABLE job_summaries;UPDATE metadata SET value='6' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnsureSchema(context.Background(), a.store); err != nil {
		t.Fatal(err)
	}
	if a.call("GET", base, token, "", nil).Body.String() != before || a.call("GET", base+"/history", token, "", nil).Body.String() != history {
		t.Fatal("schema7 rewrote private data")
	}
	if s := summaryRead(t, a, base, token); s.Revision != 0 || s.Title != "" || s.Body != "" {
		t.Fatal(s)
	}
	var version string
	if err := a.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:jobs'`).Scan(&version); err != nil || version != "7" {
		t.Fatal(version, err)
	}
}
