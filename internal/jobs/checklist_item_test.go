package jobs_test

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func itemPatch(revision int64, done bool) map[string]any {
	return map[string]any{"expected_revision": revision, "done": done}
}

func TestChecklistItemPreservesPlanAndIgnoresUnrelatedJobRevision(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	items := []progressItem{{ID: "done", Text: "  Completed 雪\n", Done: true}, {ID: "target", Text: "Remaining <literal>", Done: false}, {ID: "last", Text: "Keep me", Done: false}}
	progressOK(t, a.call("PUT", base+"/checklist", token, "plan", progressReplace(1, 0, items)), 200)
	progressOK(t, a.call("PATCH", base, token, "progress", map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": "Separate progress", "next": "Next"}}), 200)
	before := progressState(t, a)
	path := base + "/checklist/items/target"
	first := a.call("PATCH", path, token, "complete", itemPatch(1, true))
	progressOK(t, first, 200)
	if !strings.Contains(first.Body.String(), `"revision":2`) || !strings.Contains(first.Body.String(), `"job_revision":2`) || !strings.Contains(first.Body.String(), `"outcome":"updated"`) || strings.Contains(first.Body.String(), "literal") {
		t.Fatal("receipt was not compact or revisions incorrect", first.Body.String())
	}
	var notifications int
	if err := a.store.DB().QueryRow("SELECT count(*) FROM events WHERE event_type='job.checklist_replaced'").Scan(&notifications); err != nil || notifications != 2 {
		t.Fatal("item edit omitted existing live checklist-state notification", err, notifications)
	}
	after := progressState(t, a)
	if after["job_history"] != before["job_history"] || after["job_checklist_history"] != before["job_checklist_history"]+1 || after["events"] != before["events"]+1 {
		t.Fatal("checklist edit duplicated job history or omitted checklist event/history", before, after)
	}
	if got := a.call("PATCH", path, token, "complete", itemPatch(1, true)); got.Code != 200 || got.Body.String() != first.Body.String() || !reflect.DeepEqual(after, progressState(t, a)) {
		t.Fatal("exact replay repeated effects", got.Body.String())
	}
	progressOK(t, a.call("PATCH", path, token, "stale-noop", itemPatch(1, true)), 409)
	progressOK(t, a.call("PATCH", base+"/checklist/items/last", token, "complete", itemPatch(1, true)), 409)
	if !reflect.DeepEqual(after, progressState(t, a)) {
		t.Fatal("stale or key conflict wrote state")
	}
	noop := a.call("PATCH", path, token, "noop", itemPatch(2, true))
	progressOK(t, noop, 200)
	if !strings.Contains(noop.Body.String(), `"outcome":"unchanged"`) {
		t.Fatal("same completion state changed checklist")
	}
	noopState := progressState(t, a)
	delete(after, "receipts")
	delete(noopState, "receipts")
	if !reflect.DeepEqual(after, noopState) {
		t.Fatal("no-op emitted history/event")
	}
	var current progressChecklist
	decodeResponse(t, a.call("GET", base+"/checklist", token, "", nil), &current)
	items[1].Done = true
	if current.Revision != 2 || current.JobRevision != 2 || !reflect.DeepEqual(items, current.Items) {
		t.Fatal("selected update changed another item/text/order", current)
	}
	job := readWorking(t, a, base, token)
	if job.Revision != 2 || job.Progress.Summary != "Separate progress" || job.Progress.Next != "Next" || job.Checklist.Completed != 2 {
		t.Fatal("checklist edit changed unrelated job progress", job)
	}
	progressOK(t, a.call("PATCH", path, token, "reopen", itemPatch(2, false)), 200)
	if err := progressSnapshot(t, a); err != nil {
		t.Fatal(err)
	}
}

func TestChecklistItemStrictAuthorizationAndAtomicRollback(t *testing.T) {
	a, project, token, _, base := progressFixture(t)
	path := base + "/checklist/items/one"
	progressOK(t, a.call("PUT", base+"/checklist", token, "plan", progressReplace(1, 0, []progressItem{{ID: "one", Text: "Task"}})), 200)
	before := progressState(t, a)
	for i, body := range []string{`{}`, `{"expected_revision":1}`, `{"expected_revision":1,"done":null}`, `{"expected_revision":0,"done":true}`, `{"expected_revision":9223372036854775807,"done":true}`, `{"expected_revision":1,"done":true,"expected_job_revision":1}`, `{"expected_revision":1,"done":true,"done":false}`, `{"expected_revision":1,"done":"true"}`, `{"expected_revision":1,"Done":true}`} {
		r := httptest.NewRequest("PATCH", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", fmt.Sprint("invalid", i))
		w := httptest.NewRecorder()
		a.mux.ServeHTTP(w, r)
		progressOK(t, w, 400)
	}
	progressOK(t, a.call("PATCH", path, token, "", itemPatch(1, true)), 400)
	progressOK(t, a.call("PATCH", path+"?unexpected=true", token, "query", itemPatch(1, true)), 400)
	progressOK(t, a.call("PATCH", base+"/checklist/items/missing", token, "missing", itemPatch(1, true)), 404)
	readonly := a.grant(project, "item-reader", "read")
	progressOK(t, a.call("PATCH", path, readonly, "readonly", itemPatch(1, true)), 403)
	other := a.createProject("foreign-item", "example/foreign-item")
	otherToken := a.grant(other, "other-item-client", "read", "write")
	progressOK(t, a.call("PATCH", strings.Replace(path, project, other, 1), otherToken, "foreign", itemPatch(1, true)), 404)
	progressOK(t, a.call("PATCH", path, otherToken, "foreign-token", itemPatch(1, true)), 403)
	// Project/grant setup creates its own state; failed item writes never do.
	state := progressState(t, a)
	if state["job_checklist_history"] != before["job_checklist_history"] || state["job_history"] != before["job_history"] {
		t.Fatal("rejected item operation changed history")
	}
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_item_history BEFORE INSERT ON job_checklist_history BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("PATCH", path, token, "rollback", itemPatch(1, true)), 500)
	if !reflect.DeepEqual(state, progressState(t, a)) {
		t.Fatal("history failure left partial checklist/event/receipt")
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_item_history`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("PATCH", path, token, "rollback", itemPatch(1, true)), 200)
	progressOK(t, a.call("PATCH", path, readonly, "rollback", itemPatch(1, true)), 403)
}

func TestChecklistItemConcurrentCompletionHasOneWinner(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	progressOK(t, a.call("PUT", base+"/checklist", token, "plan", progressReplace(1, 0, []progressItem{{ID: "one", Text: "Task"}, {ID: "two", Text: "Other"}})), 200)
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- a.call("PATCH", base+"/checklist/items/"+id, token, "complete-"+id, itemPatch(1, true)).Code
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal("concurrent updates lost guard", counts)
	}
}
