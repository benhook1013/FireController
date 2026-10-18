package jobs_test

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

type progressItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}
type progressChecklist struct {
	JobID       string         `json:"job_id"`
	JobRevision int64          `json:"job_revision"`
	Revision    int64          `json:"revision"`
	Items       []progressItem `json:"items"`
}

func progressFixture(t *testing.T) (*testAPI, string, string, string, string) {
	t.Helper()
	a := setupAPI(t)
	p := a.createProject("progress", "example/progress")
	token := a.grant(p, "progress-client", "read", "write")
	worker := a.createWorker(p, "builder")
	job := createCheckpointTestJob(t, a, p, token)
	return a, p, token, worker, "/api/v1/projects/" + p + "/jobs/" + job.ID
}
func progressState(t *testing.T, a *testAPI) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "events", "receipts"} {
		var n int
		if err := a.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}
func progressReplace(job, revision int64, items []progressItem) map[string]any {
	return map[string]any{"expected_job_revision": job, "expected_revision": revision, "items": items}
}
func progressOK(t *testing.T, r *httptest.ResponseRecorder, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status%d want%d: %s", r.Code, status, r.Body.String())
	}
}
func TestProgressIndependentStateReplayAndStableHistory(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	lookups := a.lookups
	jobBefore := a.call("GET", base, token, "", nil).Body.String()
	historyBefore := a.call("GET", base+"/history", token, "", nil).Body.String()
	a.callWorker("POST", base+"/checkpoints", token, worker, "cp", map[string]any{"expected_job_revision": 1, "done": "prior", "next": "continue"})
	cpBefore := a.call("GET", base+"/checkpoints", token, "", nil).Body.String()
	empty := a.call("GET", base+"/checklist", token, "", nil)
	progressOK(t, empty, 200)
	var current progressChecklist
	decodeResponse(t, empty, &current)
	if current.Revision != 0 || current.JobRevision != 1 || current.Items == nil || len(current.Items) != 0 {
		t.Fatal(current)
	}
	unchanged := a.callWorker("PUT", base+"/checklist", token, worker, "initial-empty", progressReplace(1, 0, []progressItem{}))
	progressOK(t, unchanged, 200)
	if !strings.Contains(unchanged.Body.String(), `"outcome":"unchanged"`) || progressState(t, a)["job_checklist_history"] != 0 {
		t.Fatal("empty initial checklist invented history")
	}
	update := map[string]any{"expected_job_revision": 1, "body": "  Narrative <literal> 雪\n\t ", "evidence": []string{"ref-2", "ref-1"}}
	first := a.callWorker("POST", base+"/updates", token, worker, "u1", update)
	progressOK(t, first, 201)
	if strings.Contains(first.Body.String(), "Narrative") || strings.Contains(first.Body.String(), "ref-") {
		t.Fatal("receipt exposed text")
	}
	plans := []progressItem{{"stable-a", "  Original <plan> 雪 ", false}, {"stable-b", "Second", true}}
	firstPlan := a.callWorker("PUT", base+"/checklist", token, worker, "c1", progressReplace(1, 0, plans))
	progressOK(t, firstPlan, 200)
	if !strings.Contains(firstPlan.Body.String(), `"outcome":"updated"`) || strings.Contains(firstPlan.Body.String(), "Original") {
		t.Fatal(firstPlan.Body.String())
	}
	beforeNoop := progressState(t, a)
	noop := a.callWorker("PUT", base+"/checklist", token, worker, "same-content", progressReplace(1, 1, plans))
	progressOK(t, noop, 200)
	afterNoop := progressState(t, a)
	beforeNoop["receipts"]++
	if !reflect.DeepEqual(beforeNoop, afterNoop) {
		t.Fatal("identical replacement changed state/history/events")
	}
	a.callWorker("POST", base+"/updates", token, worker, "u2", map[string]any{"expected_job_revision": 1, "body": "second"})
	plans[0].Done = true
	plans[0], plans[1] = plans[1], plans[0]
	progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "c2", progressReplace(1, 1, plans)), 200)
	type archive struct {
		Items   []map[string]any `json:"items"`
		Cursor  string           `json:"cursor"`
		HasMore bool             `json:"has_more"`
	}
	for _, route := range []string{"updates", "checklist/history"} {
		r := a.call("GET", base+"/"+route+"?limit=1", token, "", nil)
		progressOK(t, r, 200)
		var page archive
		decodeResponse(t, r, &page)
		if len(page.Items) != 1 || !page.HasMore || page.Cursor == "" {
			t.Fatal(page)
		}
		if route == "updates" {
			progressOK(t, a.callWorker("POST", base+"/updates", token, worker, "u3", map[string]any{"expected_job_revision": 1, "body": "third"}), 201)
		} else {
			progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "c3", progressReplace(1, 2, []progressItem{})), 200)
		}
		r = a.call("GET", base+"/"+route+"?limit=1&cursor="+url.QueryEscape(page.Cursor), token, "", nil)
		progressOK(t, r, 200)
		decodeResponse(t, r, &page)
		if len(page.Items) != 1 || page.HasMore || page.Items[0]["worker_id"] != worker || page.Items[0]["actor_id"] == "" {
			t.Fatal(page)
		}
		if route == "updates" {
			if page.Items[0]["body"] != update["body"] || !reflect.DeepEqual(page.Items[0]["evidence"], []any{"ref-2", "ref-1"}) {
				t.Fatal("update text/order lost")
			}
		} else {
			if page.Items[0]["revision"] != float64(1) || page.Items[0]["job_revision"] != float64(1) {
				t.Fatal("history revision lost")
			}
		}
	}
	latestChecklist := a.call("GET", base+"/checklist", token, "", nil)
	decodeResponse(t, latestChecklist, &current)
	if current.Revision != 3 || len(current.Items) != 0 || current.JobRevision != 1 {
		t.Fatal("clear/history disagrees with current checklist", current)
	}
	if a.call("GET", base, token, "", nil).Body.String() != jobBefore || a.call("GET", base+"/history", token, "", nil).Body.String() != historyBefore || a.call("GET", base+"/checkpoints", token, "", nil).Body.String() != cpBefore {
		t.Fatal("progress rewrote standing job/history/checkpoint")
	}
	progressOK(t, a.call("PATCH", base, token, "brief-edit", map[string]any{"expected_revision": 1, "brief": "New current brief"}), 200)
	before := progressState(t, a)
	for _, pair := range []struct {
		path, key string
		body      any
		want      string
	}{{"updates", "u1", update, first.Body.String()}, {"checklist", "c1", progressReplace(1, 0, []progressItem{{"stable-a", "  Original <plan> 雪 ", false}, {"stable-b", "Second", true}}), firstPlan.Body.String()}} {
		method := "POST"
		status := 201
		if pair.path == "checklist" {
			method = "PUT"
			status = 200
		}
		r := a.callWorker(method, base+"/"+pair.path, token, worker, pair.key, pair.body)
		progressOK(t, r, status)
		if r.Body.String() != pair.want {
			t.Fatal("replay after advancement changed receipt")
		}
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("replay wrote new effects")
	}
	changedReplay := a.callWorker("PUT", base+"/checklist", token, worker, "c1", progressReplace(1, 0, []progressItem{{"other", "Changed original request", false}}))
	progressOK(t, changedReplay, 409)
	if !strings.Contains(changedReplay.Body.String(), "idempotency_conflict") {
		t.Fatal("changed key reuse hid behind later job guard", changedReplay.Body.String())
	}

	progressOK(t, a.callWorker("POST", base+"/updates", token, worker, "stale-update", update), 409)
	progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "stale-plan", progressReplace(1, 3, []progressItem{})), 409)
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("stale guards wrote effects")
	}
	var payloads []string
	rows, err := a.store.DB().Query(`SELECT payload_json FROM events WHERE project_id=? AND event_type IN ('job.update_appended','job.checklist_replaced')`, p)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b string
		rows.Scan(&b)
		payloads = append(payloads, b)
	}
	rows.Close()
	for _, b := range payloads {
		if strings.Contains(b, "Narrative") || strings.Contains(b, "Original") || strings.Contains(b, "ref-") {
			t.Fatal("event content leak")
		}
	}
	if a.lookups != lookups {
		t.Fatal("progress did provider work")
	}
}

func TestProgressGuardsIsolationRacesAndRollback(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	readOnly := a.grant(p, "progress-reader", "read")
	otherP := a.createProject("other", "example/other")
	other := a.grant(otherP, "other-client", "read", "write")
	items := []progressItem{{"one", "Plan", false}}
	body := progressReplace(1, 0, items)
	for _, route := range []string{"updates", "checklist", "checklist/history"} {
		progressOK(t, a.call("GET", base+"/"+route, other, "", nil), 403)
	}
	progressOK(t, a.callWorker("PUT", base+"/checklist", readOnly, worker, "readonly", body), 403)
	progressOK(t, a.callWorker("POST", base+"/updates", readOnly, worker, "readonly-update", map[string]any{"expected_job_revision": 1, "body": "forbidden"}), 403)
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := a.callWorker("PUT", base+"/checklist", token, worker, fmt.Sprint("race-", i), progressReplace(1, 0, []progressItem{{"one", fmt.Sprint("Plan", i), false}}))
			codes <- r.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal("checklist guards lost race", counts)
	}
	current := a.call("GET", base+"/checklist", token, "", nil)
	var view progressChecklist
	decodeResponse(t, current, &view)
	if view.Revision != 1 {
		t.Fatal(view)
	}
	before := progressState(t, a)
	stale := a.callWorker("PUT", base+"/checklist", token, worker, "stale", body)
	progressOK(t, stale, 409)
	if !strings.Contains(stale.Body.String(), "checklist_revision_conflict") || !strings.Contains(stale.Body.String(), `"current_revision":1`) {
		t.Fatal(stale.Body.String())
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("stale state wrote")
	}
	for _, kind := range []string{"updates", "checklist"} {
		if _, err := a.store.DB().Exec(`CREATE TRIGGER progress_event_failure BEFORE INSERT ON events WHEN NEW.event_type IN ('job.update_appended','job.checklist_replaced') BEGIN SELECT RAISE(ABORT,'forced rollback');END;`); err != nil {
			t.Fatal(err)
		}
		before = progressState(t, a)
		method := "POST"
		request := any(map[string]any{"expected_job_revision": 1, "body": "atomic update"})
		status := 201
		if kind == "checklist" {
			method = "PUT"
			request = progressReplace(1, 1, items)
			status = 200
		}
		key := "rollback-" + kind
		r := a.callWorker(method, base+"/"+kind, token, worker, key, request)
		if r.Code < 500 {
			t.Fatal("event failure did not abort", r.Code)
		}
		if !reflect.DeepEqual(before, progressState(t, a)) {
			t.Fatal("rollback left orphan rows/events/receipt")
		}
		if _, err := a.store.DB().Exec(`DROP TRIGGER progress_event_failure`); err != nil {
			t.Fatal(err)
		}
		progressOK(t, a.callWorker(method, base+"/"+kind, token, worker, key, request), status)
	}
	before = progressState(t, a)
	cursorPage := a.call("GET", base+"/checklist/history?limit=1", token, "", nil)
	var page struct {
		Cursor string `json:"cursor"`
	}
	decodeResponse(t, cursorPage, &page)
	if page.Cursor == "" {
		t.Fatal("fixture requires cursor")
	}
	progressOK(t, a.call("GET", base+"/updates?cursor="+url.QueryEscape(page.Cursor), token, "", nil), 400)
	progressOK(t, a.call("GET", base+"/checklist/history?cursor="+url.QueryEscape(page.Cursor), readOnly, "", nil), 400)
	for _, route := range []string{"updates", "checklist", "checklist/history"} {
		progressOK(t, a.call("GET", strings.Replace(base, "/jobs/", "/jobs/missing-", 1)+"/"+route, token, "", nil), 404)
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("progress reads mutated database")
	}
	if _, err := a.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=(SELECT token_sha256 FROM credentials WHERE principal_id=(SELECT actor_id FROM job_updates LIMIT 1))`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("GET", base+"/checklist", token, "", nil), 401)
}

func TestProgressStrictContentLimitsAndReadBudgets(t *testing.T) {
	a, _, token, worker, base := progressFixture(t)
	update := map[string]any{"expected_job_revision": 1, "body": "valid"}
	invalidUpdates := []map[string]any{
		{"expected_job_revision": 0, "body": "valid"}, {"expected_job_revision": nil, "body": "valid"}, {"expected_job_revision": 1, "body": "  "},
		{"expected_job_revision": 1, "body": "bad\x1b[31m"}, {"expected_job_revision": 1, "body": "bad\x7f"}, {"expected_job_revision": 1, "body": strings.Repeat("x", 8193)},
		{"expected_job_revision": 1, "body": "ok", "evidence": []string{" "}}, {"expected_job_revision": 1, "body": "ok", "evidence": []string{strings.Repeat("x", 1025)}},
		{"expected_job_revision": 1, "body": strings.Repeat("x", 8192), "evidence": []string{strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), strings.Repeat("x", 1024), "x"}},
	}
	before := progressState(t, a)
	for _, raw := range [][]byte{[]byte(`{"expected_job_revision":1,"body":"bad\ud800"}`), []byte("{\"expected_job_revision\":1,\"body\":\"bad\xff\"}"), []byte(`{"expected_job_revision":1,"Body":"alias"}`), []byte(`{"expected_job_revision":1,"body":"one","body":"two"}`)} {
		r := httptest.NewRequest("POST", base+"/updates", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Instance", a.store.InstanceID())
		r.Header.Set("X-FC-Worker", worker)
		r.Header.Set("Idempotency-Key", "invalid-wire")
		w := httptest.NewRecorder()
		a.mux.ServeHTTP(w, r)
		progressOK(t, w, 400)
	}
	for i, b := range invalidUpdates {
		progressOK(t, a.callWorker("POST", base+"/updates", token, worker, fmt.Sprint("invalid-u", i), b), 400)
	}
	invalidPlans := []any{
		map[string]any{"expected_job_revision": 1, "items": []progressItem{}}, progressReplace(1, -1, []progressItem{}), progressReplace(1, math.MaxInt64, []progressItem{}),
		map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": nil},
		map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": []any{map[string]any{"id": "one", "text": "required done"}}},
		map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": []any{map[string]any{"id": "one", "text": "required done", "done": nil}}},
		progressReplace(1, 0, []progressItem{{"one", "one", false}, {"one", "duplicate", true}}), progressReplace(1, 0, []progressItem{{"unicode-雪", "bad identity", false}}),
		progressReplace(1, 0, []progressItem{{"one", "bad\x00", false}}), progressReplace(1, 0, []progressItem{{"one", strings.Repeat("x", 1025), false}}),
	}
	aggregate := make([]progressItem, 100)
	for i := range aggregate {
		aggregate[i] = progressItem{fmt.Sprint("i", i), strings.Repeat("x", 200), false}
	}
	invalidPlans = append(invalidPlans, progressReplace(1, 0, aggregate))
	tooMany := make([]progressItem, 101)
	for i := range tooMany {
		tooMany[i] = progressItem{fmt.Sprint("i", i), "x", false}
	}
	invalidPlans = append(invalidPlans, progressReplace(1, 0, tooMany))
	for i, b := range invalidPlans {
		progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, fmt.Sprint("invalid-c", i), b), 400)
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("invalid progress wrote effects")
	}
	// Missing/null/empty evidence normalize to the same retained key.
	r := a.callWorker("POST", base+"/updates", token, worker, "normalize", update)
	progressOK(t, r, 201)
	for _, e := range []any{nil, []string{}} {
		update["evidence"] = e
		replay := a.callWorker("POST", base+"/updates", token, worker, "normalize", update)
		progressOK(t, replay, 201)
		if replay.Body.String() != r.Body.String() {
			t.Fatal("equivalent evidence replay changed receipt")
		}
	}
	update["body"] = "changed"
	progressOK(t, a.callWorker("POST", base+"/updates", token, worker, "normalize", update), 409)
	// The maximum raw update can expand under JSON escaping. Archives fail
	// explicitly at4MiB and allow the caller to request fewer entries.
	refs := make([]string, 8)
	for i := range refs {
		refs[i] = strings.Repeat("<", 1024)
	}
	for i := 0; i < 44; i++ {
		progressOK(t, a.callWorker("POST", base+"/updates", token, worker, fmt.Sprint("large", i), map[string]any{"expected_job_revision": 1, "body": strings.Repeat("<", 8192), "evidence": refs}), 201)
	}
	before = progressState(t, a)
	oversize := a.call("GET", base+"/updates?limit=100", token, "", nil)
	progressOK(t, oversize, 413)
	if !strings.Contains(oversize.Body.String(), "response_too_large") {
		t.Fatal(oversize.Body.String())
	}
	progressOK(t, a.call("GET", base+"/updates?limit=1", token, "", nil), 200)
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("history budget/read changed effects")
	}
	for _, route := range []string{"updates?limit=1&limit=2", "checklist?cursor=x", "checklist/history?limit=101"} {
		progressOK(t, a.call("GET", base+"/"+route, token, "", nil), 400)
	}
}

func TestProgressRestartAndJobsV2MigrationPreserveStandingHistory(t *testing.T) {
	a, _, token, worker, base := progressFixture(t)
	progressOK(t, a.callWorker("POST", base+"/checkpoints", token, worker, "cp", map[string]any{"expected_job_revision": 1, "done": "checkpoint", "next": "continue"}), 201)
	// Reconstruct the truthful preceding schema in this disposable fixture.
	if _, err := a.store.DB().Exec(`DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history; DROP TABLE worker_pause_state; DROP TABLE worker_primary_job_history; DROP TABLE worker_primary_jobs; DROP TABLE job_updates;DROP TABLE job_checklists;DROP TABLE job_checklist_history;UPDATE metadata SET value='2' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	stable := map[string]string{}
	for _, route := range []string{"", "/history", "/checkpoints"} {
		stable[route] = a.call("GET", base+route, token, "", nil).Body.String()
	}
	var path string
	if err := a.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopen := func() {
		s, err := core.Open(context.Background(), filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err = jobs.EnsureSchema(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		a.store = s
		a.mux = http.NewServeMux()
		jobs.Register(a.mux, s)
	}
	reopen()
	for route, want := range stable {
		if a.call("GET", base+route, token, "", nil).Body.String() != want {
			t.Fatal("migration changed standing history", route)
		}
	}
	body := map[string]any{"expected_job_revision": 1, "body": "retained update"}
	prior := a.callWorker("POST", base+"/updates", token, worker, "restart-update", body)
	progressOK(t, prior, 201)
	items := []progressItem{{"one", "Retained plan", true}}
	progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "restart-plan", progressReplace(1, 0, items)), 200)
	expected := map[string]string{}
	for _, route := range []string{"/updates", "/checklist", "/checklist/history"} {
		expected[route] = a.call("GET", base+route, token, "", nil).Body.String()
	}
	a.store.Close()
	reopen()
	before := progressState(t, a)
	for route, want := range expected {
		if a.call("GET", base+route, token, "", nil).Body.String() != want {
			t.Fatal("restart lost progress", route)
		}
	}
	replay := a.callWorker("POST", base+"/updates", token, worker, "restart-update", body)
	progressOK(t, replay, 201)
	if !bytes.Equal(prior.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("restart changed original receipt")
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("restart reads/replay wrote effects")
	}
}

func TestProgressRevisionBoundsPreserveStoredMaxAndExactReplay(t *testing.T) {
	a, _, token, worker, base := progressFixture(t)
	body := progressReplace(1, 0, []progressItem{{"one", "Plan", false}})
	original := a.callWorker("PUT", base+"/checklist", token, worker, "original", body)
	progressOK(t, original, 200)
	progressOK(t, a.callWorker("POST", base+"/updates", token, worker, "max-job", map[string]any{"expected_job_revision": int64(math.MaxInt64), "body": "Valid guard, obsolete job"}), 409)
	progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "max-job-checklist", progressReplace(math.MaxInt64, 1, []progressItem{})), 409)
	if _, err := a.store.DB().Exec(`UPDATE job_checklists SET revision=?`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	before := progressState(t, a)
	r := a.call("GET", base+"/checklist", token, "", nil)
	progressOK(t, r, 200)
	var value progressChecklist
	decodeResponse(t, r, &value)
	if value.Revision != math.MaxInt64 {
		t.Fatal("stored max revision became unreadable")
	}
	progressOK(t, a.callWorker("PUT", base+"/checklist", token, worker, "overflow", progressReplace(1, math.MaxInt64, []progressItem{})), 400)
	replay := a.callWorker("PUT", base+"/checklist", token, worker, "original", body)
	progressOK(t, replay, 200)
	if replay.Body.String() != original.Body.String() || !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("overflow/read/exactreplay changed retained state")
	}
}
