package jobs_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

type primaryValue struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	JobID    string `json:"job_id"`
}
type primaryEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	JobID         string `json:"job_id"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	Reason        string `json:"reason"`
	CreatedAt     string `json:"created_at"`
}
type primaryPage struct {
	Items   []primaryEntry `json:"items"`
	Cursor  string         `json:"cursor"`
	HasMore bool           `json:"has_more"`
}

func primaryFixture(t *testing.T) (*testAPI, string, string, string, string, string, string) {
	t.Helper()
	a := setupAPI(t)
	p := a.createProject("focus", "synthetic/focus")
	token := a.grant(p, "focus-writer", "read", "write")
	worker := a.createWorker(p, "Builder")
	other := a.createWorker(p, "Other")
	create := func(title string) string {
		r := a.call("POST", "/api/v1/projects/"+p+"/jobs", token, "create-"+title, map[string]any{"title": title, "brief": "Standing instruction", "worker_id": worker})
		progressOK(t, r, 201)
		var receipt receipt
		decodeResponse(t, r, &receipt)
		return receipt.ID
	}
	return a, p, token, worker, other, create("one"), create("two")
}
func primaryPath(p, w string) string {
	return "/api/v1/projects/" + p + "/workers/" + w + "/primary-job"
}
func primaryRequest(revision int64, job string) map[string]any {
	return map[string]any{"expected_revision": revision, "job_id": job}
}
func primaryRead(t *testing.T, a *testAPI, path, token string) primaryValue {
	t.Helper()
	r := a.call("GET", path, token, "", nil)
	progressOK(t, r, 200)
	var v primaryValue
	decodeResponse(t, r, &v)
	return v
}
func primaryCounts(t *testing.T, a *testAPI) map[string]int {
	t.Helper()
	out := progressState(t, a)
	for _, n := range []string{"worker_primary_jobs", "worker_primary_job_history"} {
		var count int
		if err := a.store.DB().QueryRow("SELECT count(*) FROM " + n).Scan(&count); err != nil {
			t.Fatal(err)
		}
		out[n] = count
	}
	return out
}
func primaryError(t *testing.T, r *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	progressOK(t, r, status)
	var v struct {
		Error struct {
			Code    string           `json:"code"`
			Details map[string]int64 `json:"details"`
		} `json:"error"`
	}
	decodeResponse(t, r, &v)
	if v.Error.Code != code {
		t.Fatalf("error %s want %s: %s", v.Error.Code, code, r.Body.String())
	}
}
func TestPrimaryFocusIndependentLifecycleAndAssignment(t *testing.T) {
	a, p, token, w, other, j1, j2 := primaryFixture(t)
	path := primaryPath(p, w)
	lookups := a.lookups
	before := primaryCounts(t, a)
	if v := primaryRead(t, a, path, token); v != (primaryValue{WorkerID: w}) {
		t.Fatal(v)
	}
	h := a.call("GET", path+"/history", token, "", nil)
	progressOK(t, h, 200)
	var empty primaryPage
	decodeResponse(t, h, &empty)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal(empty)
	}
	if !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("reads changed state")
	}
	jobBefore := map[string]string{}
	for _, j := range []string{j1, j2} {
		jobBefore[j] = a.call("GET", "/api/v1/projects/"+p+"/jobs/"+j, token, "", nil).Body.String()
	}
	first := a.callWorker("PUT", path, token, other, "select-first", primaryRequest(0, j1))
	progressOK(t, first, 200)
	var receipt receipt
	decodeResponse(t, first, &receipt)
	if receipt.ID != w || receipt.Revision != 1 || receipt.Outcome != "updated" {
		t.Fatal(receipt)
	}
	for _, j := range []string{j1, j2} {
		if a.call("GET", "/api/v1/projects/"+p+"/jobs/"+j, token, "", nil).Body.String() != jobBefore[j] {
			t.Fatal("selection changed job")
		}
	}
	before = primaryCounts(t, a)
	progressOK(t, a.call("PUT", path, token, "same", primaryRequest(1, j1)), 200)
	after := primaryCounts(t, a)
	before["receipts"]++
	if !reflect.DeepEqual(before, after) {
		t.Fatal("same selection changed state", before, after)
	}
	progressOK(t, a.call("PUT", path, token, "switch", primaryRequest(1, j2)), 200)
	before = primaryCounts(t, a)
	replay := a.callWorker("PUT", path, token, other, "select-first", primaryRequest(0, j1))
	progressOK(t, replay, 200)
	if replay.Body.String() != first.Body.String() || !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("exact replay changed result/effects")
	}
	primaryError(t, a.callWorker("PUT", path, token, other, "select-first", primaryRequest(0, j2)), 409, "idempotency_conflict")
	jobPath := "/api/v1/projects/" + p + "/jobs/" + j2
	progressOK(t, a.call("PATCH", jobPath, token, "complete", map[string]any{"expected_revision": 1, "status": "completed"}), 200)
	if v := primaryRead(t, a, path, token); v.JobID != j2 || v.Revision != 2 {
		t.Fatal("status silently cleared focus", v)
	}
	progressOK(t, a.call("PUT", path, token, "clear", primaryRequest(2, "")), 200)
	before = primaryCounts(t, a)
	progressOK(t, a.call("PUT", path, token, "clear-again", primaryRequest(3, "")), 200)
	before["receipts"]++
	if !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("empty clear changed history")
	}
	progressOK(t, a.call("PUT", path, token, "completed-focus", primaryRequest(3, j2)), 200)
	progressOK(t, a.callWorker("PATCH", jobPath, token, other, "transfer", map[string]any{"expected_revision": 2, "worker_id": other}), 200)
	if v := primaryRead(t, a, path, token); v.Revision != 5 || v.JobID != "" {
		t.Fatal("transfer did not clear", v)
	}
	if v := primaryRead(t, a, primaryPath(p, other), token); v.Revision != 0 || v.JobID != "" {
		t.Fatal("transfer selected receiver", v)
	}
	progressOK(t, a.call("PUT", path, token, "refocus", primaryRequest(5, j1)), 200)
	before = primaryCounts(t, a)
	primaryError(t, a.call("PATCH", "/api/v1/projects/"+p+"/jobs/"+j1, token, "stale-transfer", map[string]any{"expected_revision": 2, "worker_id": other}), 409, "revision_conflict")
	if !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("stale transfer cleared focus")
	}
	progressOK(t, a.call("PATCH", "/api/v1/projects/"+p+"/jobs/"+j1, token, "unassign", map[string]any{"expected_revision": 1, "worker_id": ""}), 200)
	if v := primaryRead(t, a, path, token); v.JobID != "" || v.Revision != 7 {
		t.Fatal(v)
	}
	history := a.call("GET", path+"/history", token, "", nil)
	progressOK(t, history, 200)
	var page primaryPage
	decodeResponse(t, history, &page)
	if len(page.Items) != 7 || page.Items[0].Reason != "job_reassigned" || page.Items[6].ActorWorkerID != other || page.Items[6].ActorID == "" {
		t.Fatal(page)
	}
	if a.lookups != lookups {
		t.Fatal("focus called provider")
	}
}

func TestPrimaryFocusValidationIsolationAndCursorBinding(t *testing.T) {
	a, p, token, w, other, j1, j2 := primaryFixture(t)
	path := primaryPath(p, w)
	before := primaryCounts(t, a)
	staleEmpty := a.call("PUT", path, token, "stale-empty", primaryRequest(1, j1))
	primaryError(t, staleEmpty, 409, "primary_job_revision_conflict")
	if !strings.Contains(staleEmpty.Body.String(), `"current_revision":0`) {
		t.Fatal("empty focus conflict omitted zero revision", staleEmpty.Body.String())
	}
	// An explicit initial clear is a no-op and creates no state/history/event.
	initial := a.call("PUT", path, token, "empty", primaryRequest(0, ""))
	progressOK(t, initial, 200)
	var empty receipt
	decodeResponse(t, initial, &empty)
	if empty.Revision != 0 || empty.Outcome != "unchanged" {
		t.Fatal(empty)
	}
	before["receipts"]++
	if !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("initial clear wrote state")
	}
	for i, raw := range []string{`{}`, `{"expected_revision":null,"job_id":""}`, `{"expected_revision":0}`, `{"expected_revision":0,"job_id":null}`, `{"expected_revision":0,"job_id":"bad/path"}`, `{"expected_revision":0,"job_id":" bad"}`, `{"expected_revision":0,"job_id":"","extra":true}`, `{"expected_revision":0,"job_id":"","job_id":""}`, `{"Expected_Revision":0,"job_id":""}`, `{"expected_revision":-1,"job_id":""}`, `{"expected_revision":9223372036854775807,"job_id":""}`} {
		r := a.call("PUT", path, token, fmt.Sprint("bad", i), json.RawMessage(raw))
		if r.Code != 400 {
			t.Fatalf("raw invalid admitted %s: %d %s", raw, r.Code, r.Body.String())
		}
	}
	if !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("invalid body wrote state")
	}
	for _, method := range []string{"GET", "PUT"} {
		var body any
		if method == "PUT" {
			body = primaryRequest(0, "")
		}
		primaryError(t, a.call(method, primaryPath(p, "worker_missing"), token, "missing-worker", body), 404, "worker_not_found")
	}
	primaryError(t, a.call("PUT", path, token, "missing-job", primaryRequest(0, "job_missing")), 404, "job_not_found")
	primaryError(t, a.call("PUT", primaryPath(p, other), token, "wrong-assignment", primaryRequest(0, j1)), 409, "primary_job_assignment_conflict")
	unassigned := a.call("POST", "/api/v1/projects/"+p+"/jobs", token, "unassigned-job", map[string]any{"title": "Unassigned"})
	progressOK(t, unassigned, 201)
	var noWorker receipt
	decodeResponse(t, unassigned, &noWorker)
	primaryError(t, a.call("PUT", path, token, "unassigned-selection", primaryRequest(0, noWorker.ID)), 409, "primary_job_assignment_conflict")
	// Project authority is retained: target and authenticated attribution can differ.
	progressOK(t, a.callWorker("PUT", path, token, other, "one", primaryRequest(0, j1)), 200)
	conflict := a.call("PUT", path, token, "stale", primaryRequest(0, j2))
	primaryError(t, conflict, 409, "primary_job_revision_conflict")
	if !strings.Contains(conflict.Body.String(), `"current_revision":1`) {
		t.Fatal(conflict.Body.String())
	}
	progressOK(t, a.call("PUT", path, token, "two", primaryRequest(1, j2)), 200)
	progressOK(t, a.call("PUT", path, token, "three", primaryRequest(2, "")), 200)
	readOnly := a.grant(p, "focus-readonly", "read")
	primaryError(t, a.call("PUT", path, readOnly, "readonly", primaryRequest(3, j1)), 403, "forbidden")
	progressOK(t, a.call("GET", path, readOnly, "", nil), 200)
	p2 := a.createProject("other-focus", "synthetic/other-focus")
	foreign := a.grant(p2, "foreign-focus", "read", "write")
	fw := a.createWorker(p2, "Builder")
	primaryError(t, a.call("GET", path, foreign, "", nil), 403, "forbidden")
	primaryError(t, a.call("PUT", path, foreign, "cross", primaryRequest(3, j1)), 403, "forbidden")
	primaryError(t, a.call("PUT", primaryPath(p, fw), token, "cross-worker", primaryRequest(0, j1)), 404, "worker_not_found")
	foreignJob := a.call("POST", "/api/v1/projects/"+p2+"/jobs", foreign, "foreign-job", map[string]any{"title": "Foreign", "worker_id": fw})
	progressOK(t, foreignJob, 201)
	var fj receipt
	decodeResponse(t, foreignJob, &fj)
	primaryError(t, a.call("PUT", path, token, "foreign-job", primaryRequest(3, fj.ID)), 404, "job_not_found")
	pageResult := a.call("GET", path+"/history?limit=2", token, "", nil)
	progressOK(t, pageResult, 200)
	var page primaryPage
	decodeResponse(t, pageResult, &page)
	if len(page.Items) != 2 || page.Items[0].Revision != 3 || page.Items[1].Revision != 2 || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	progressOK(t, a.call("PUT", path, token, "four", primaryRequest(3, j1)), 200)
	cursor := "?limit=2&cursor=" + url.QueryEscape(page.Cursor)
	next := a.call("GET", path+"/history"+cursor, token, "", nil)
	progressOK(t, next, 200)
	var rest primaryPage
	decodeResponse(t, next, &rest)
	if len(rest.Items) != 1 || rest.Items[0].Revision != 1 || rest.HasMore || rest.Cursor != "" || rest.Items[0].ActorWorkerID != other {
		t.Fatal(rest)
	}
	for _, tc := range []struct{ path, token string }{{primaryPath(p, other) + "/history" + cursor, token}, {path + "/history" + cursor, a.admin}, {primaryPath(p2, fw) + "/history" + cursor, foreign}, {path + "/history?limit=101", token}, {path + "/history?limit=1&limit=2", token}, {path + "/history?cursor=%zz", token}, {path + "?limit=1", token}} {
		progressOK(t, a.call("GET", tc.path, tc.token, "", nil), 400)
	}
	// Revocation applies even to an exact historical receipt.
	sum := sha256.Sum256([]byte(token))
	var credential string
	if err := a.store.DB().QueryRow(`SELECT id FROM credentials WHERE token_sha256=?`, fmt.Sprintf("%x", sum)).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("DELETE", "/api/v1/projects/"+p+"/credentials/"+credential, a.admin, "revoke", nil), 200)
	primaryError(t, a.call("PUT", path, token, "one", primaryRequest(0, j1)), 401, "unauthorized")
}

func TestPrimaryFocusRaceRollbackAndExhaustion(t *testing.T) {
	a, p, token, w, other, j1, j2 := primaryFixture(t)
	path := primaryPath(p, w)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, job := range []string{j1, j2} {
		wg.Add(1)
		go func(i int, job string) {
			defer wg.Done()
			<-start
			responses <- a.call("PUT", path, token, fmt.Sprint("race", i), primaryRequest(0, job))
		}(i, job)
	}
	close(start)
	wg.Wait()
	close(responses)
	successes, conflicts := 0, 0
	for r := range responses {
		switch r.Code {
		case 200:
			successes++
		case 409:
			primaryError(t, r, 409, "primary_job_revision_conflict")
			conflicts++
		default:
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	selected := primaryRead(t, a, path, token)
	before := primaryCounts(t, a)
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_primary BEFORE INSERT ON events WHEN NEW.event_type='worker.primary_job_changed' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := a.call("PUT", path, token, "rollback-key", primaryRequest(1, ""))
	if failed.Code < 500 {
		t.Fatal("event fault was not propagated", failed.Code)
	}
	if !reflect.DeepEqual(before, primaryCounts(t, a)) || primaryRead(t, a, path, token) != selected {
		t.Fatal("partial explicit focus mutation")
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_primary;CREATE TRIGGER fail_primary_job BEFORE INSERT ON events WHEN NEW.event_type='job.updated' BEGIN SELECT RAISE(ABORT,'fixture failure after both resources staged');END`); err != nil {
		t.Fatal(err)
	}
	jobPath := "/api/v1/projects/" + p + "/jobs/" + selected.JobID
	originalJob := a.call("GET", jobPath, token, "", nil).Body.String()
	failed = a.call("PATCH", jobPath, token, "rollback-transfer", map[string]any{"expected_revision": 1, "worker_id": other})
	if failed.Code < 500 {
		t.Fatal(failed.Code)
	}
	if !reflect.DeepEqual(before, primaryCounts(t, a)) || a.call("GET", jobPath, token, "", nil).Body.String() != originalJob {
		t.Fatal("partial job/focus clear")
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_primary_job`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("PUT", path, token, "rollback-key", primaryRequest(1, "")), 200)
	progressOK(t, a.call("PUT", path, token, "select-again", primaryRequest(2, j1)), 200)
	if _, err := a.store.DB().Exec(`UPDATE worker_primary_jobs SET revision=? WHERE project_id=? AND worker_id=?`, int64(math.MaxInt64-1), p, w); err != nil {
		t.Fatal(err)
	}
	first := a.call("PUT", path, token, "last-revision", primaryRequest(math.MaxInt64-1, j2))
	progressOK(t, first, 200)
	if v := primaryRead(t, a, path, token); v.Revision != math.MaxInt64 || v.JobID != j2 {
		t.Fatal(v)
	}
	before = primaryCounts(t, a)
	primaryError(t, a.call("PUT", path, token, "overflow-direct", primaryRequest(math.MaxInt64, j2)), 400, "invalid_revision")
	replay := a.call("PUT", path, token, "last-revision", primaryRequest(math.MaxInt64-1, j2))
	progressOK(t, replay, 200)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("max stored revision blocked replay")
	}
	jobPath = "/api/v1/projects/" + p + "/jobs/" + j2
	originalJob = a.call("GET", jobPath, token, "", nil).Body.String()
	primaryError(t, a.call("PATCH", jobPath, token, "overflow-transfer", map[string]any{"expected_revision": 1, "worker_id": other}), 409, "primary_job_revision_exhausted")
	if !reflect.DeepEqual(before, primaryCounts(t, a)) || a.call("GET", jobPath, token, "", nil).Body.String() != originalJob {
		t.Fatal("overflow changed job/focus/receipt/event")
	}
}

func TestPrimaryFocusRestartAndJobsThreeMigration(t *testing.T) {
	a, p, token, w, _, j1, _ := primaryFixture(t)
	path := primaryPath(p, w)
	jobPath := "/api/v1/projects/" + p + "/jobs/" + j1
	standing := a.call("GET", jobPath, token, "", nil).Body.String()
	history := a.call("GET", jobPath+"/history", token, "", nil).Body.String()
	if _, err := a.store.DB().Exec(`DROP TABLE job_progress_history; DROP TABLE job_progress; DROP TABLE job_summary_history; DROP TABLE job_summaries; DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history; DROP TABLE worker_pause_state; DROP TABLE worker_primary_job_history;DROP TABLE worker_primary_jobs;UPDATE metadata SET value='3' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	var dbfile string
	if err := a.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbfile); err != nil {
		t.Fatal(err)
	}
	reopen := func() {
		if err := a.store.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := core.Open(context.Background(), filepath.Dir(dbfile))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err = jobs.EnsureSchema(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		a.store = s
		a.mux = server.New(s)
		jobs.Register(a.mux, s)
	}
	reopen()
	if primaryRead(t, a, path, token).Revision != 0 {
		t.Fatal("migration invented focus")
	}
	if a.call("GET", jobPath, token, "", nil).Body.String() != standing || a.call("GET", jobPath+"/history", token, "", nil).Body.String() != history {
		t.Fatal("jobs3 migration rewrote standing state")
	}
	original := a.call("PUT", path, token, "restart-key", primaryRequest(0, j1))
	progressOK(t, original, 200)
	want := a.call("GET", path+"/history", token, "", nil).Body.String()
	before := primaryCounts(t, a)
	reopen()
	if v := primaryRead(t, a, path, token); v.JobID != j1 || v.Revision != 1 {
		t.Fatal(v)
	}
	if a.call("GET", path+"/history", token, "", nil).Body.String() != want {
		t.Fatal("history lost on restart")
	}
	replay := a.call("PUT", path, token, "restart-key", primaryRequest(0, j1))
	progressOK(t, replay, 200)
	if replay.Body.String() != original.Body.String() || !reflect.DeepEqual(before, primaryCounts(t, a)) {
		t.Fatal("restart replay changed ledger")
	}
}

func TestPrimaryFocusAssignmentRaceCannotLeaveInvalidSelection(t *testing.T) {
	a, p, token, w, other, j1, j2 := primaryFixture(t)
	path := primaryPath(p, w)
	progressOK(t, a.call("PUT", path, token, "first", primaryRequest(0, j1)), 200)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var selection, transfer *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		selection = a.call("PUT", path, token, "switch-concurrent", primaryRequest(1, j2))
	}()
	go func() {
		defer wg.Done()
		<-start
		transfer = a.call("PATCH", "/api/v1/projects/"+p+"/jobs/"+j2, token, "transfer-concurrent", map[string]any{"expected_revision": 1, "worker_id": other})
	}()
	close(start)
	wg.Wait()
	progressOK(t, transfer, 200)
	if selection.Code != 200 && selection.Code != 409 {
		t.Fatal(selection.Code, selection.Body.String())
	}
	current := primaryRead(t, a, path, token)
	if current.JobID == j2 {
		t.Fatal("transferred job remained primary", current)
	}
	if selection.Code == 200 {
		if current.JobID != "" || current.Revision != 3 {
			t.Fatal("successful switch was not atomically cleared", current)
		}
	} else {
		primaryError(t, selection, 409, "primary_job_assignment_conflict")
		if current.JobID != j1 || current.Revision != 1 {
			t.Fatal("failed switch altered old focus", current)
		}
	}
}

func TestPrimaryFocusDoesNotDependOnJobStatus(t *testing.T) {
	for _, status := range []string{"parked", "blocked", "completed"} {
		t.Run(status, func(t *testing.T) {
			a, p, token, w, _, j1, j2 := primaryFixture(t)
			path := primaryPath(p, w)
			job := "/api/v1/projects/" + p + "/jobs/" + j1
			progressOK(t, a.call("PATCH", job, token, "set-status", map[string]any{"expected_revision": 1, "status": status}), 200)
			before := a.call("GET", job, token, "", nil).Body.String()
			progressOK(t, a.call("PUT", path, token, "focus-status", primaryRequest(0, j1)), 200)
			if a.call("GET", job, token, "", nil).Body.String() != before {
				t.Fatal("focus edited nonactive job")
			}
			progressOK(t, a.call("PATCH", job, token, "resume-status", map[string]any{"expected_revision": 2, "status": "active"}), 200)
			if v := primaryRead(t, a, path, token); v.JobID != j1 || v.Revision != 1 {
				t.Fatal("status change edited focus", v)
			}
			var otherJob jobValue
			decodeResponse(t, a.call("GET", "/api/v1/projects/"+p+"/jobs/"+j2, token, "", nil), &otherJob)
			if otherJob.Status != "active" || otherJob.Revision != 1 {
				t.Fatal("focus restricted concurrent active work", otherJob)
			}
		})
	}
}
