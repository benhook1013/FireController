package jobs_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/jobs"
)

type workingProgress struct {
	Summary  string   `json:"summary"`
	Next     string   `json:"next"`
	Blocker  string   `json:"blocker"`
	Evidence []string `json:"evidence"`
}
type workingJob struct {
	jobValue
	Progress  workingProgress `json:"progress"`
	Checklist struct {
		Revision  int64          `json:"revision"`
		Remaining []progressItem `json:"remaining"`
		Completed int            `json:"completed"`
		Total     int            `json:"total"`
	} `json:"checklist"`
	Notes notesPageValue `json:"notes"`
}

func readWorking(t *testing.T, a *testAPI, base, token string) workingJob {
	t.Helper()
	r := a.call("GET", base, token, "", nil)
	progressOK(t, r, 200)
	var j workingJob
	decodeResponse(t, r, &j)
	return j
}
func progressSnapshot(t *testing.T, a *testAPI) error {
	t.Helper()
	tx, err := a.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return jobs.ValidateProgressSnapshot(t.Context(), tx)
}
func TestUnifiedProgressPartialClearNoopReplayAndSingleJobRevision(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	initial := readWorking(t, a, base, token)
	if initial.Progress.Evidence == nil || initial.Progress.Summary != "" || initial.Checklist.Remaining == nil || initial.Notes.Items == nil {
		t.Fatal("default read omitted explicit empty collections")
	}
	req := map[string]any{"expected_revision": 1, "status": "blocked", "worker_id": worker, "brief": "Private definition", "progress": map[string]any{"summary": "  Current <literal> 雪\n", "next": "continue", "blocker": "waiting", "evidence": []string{"ref-one"}}}
	first := a.callWorker("PATCH", base, token, worker, "one-working-write", req)
	progressOK(t, first, 200)
	current := readWorking(t, a, base, token)
	if current.Revision != 2 || current.Status != "blocked" || current.WorkerID != worker || current.Progress.Next != "continue" || current.Progress.Summary != "  Current <literal> 雪\n" {
		t.Fatal("atomic job/progress update changed supplied bytes")
	}
	if bytes.Contains(first.Body.Bytes(), []byte("literal")) {
		t.Fatal("receipt leaked progress")
	}
	state := contextState(t, a)
	replay := a.callWorker("PATCH", base, token, worker, "one-working-write", req)
	if replay.Code != 200 || replay.Body.String() != first.Body.String() || !reflect.DeepEqual(state, contextState(t, a)) {
		t.Fatal("replay repeated a write")
	}
	clear := a.call("PATCH", base, token, "clear", map[string]any{"expected_revision": 2, "progress": map[string]any{"next": "", "blocker": "", "evidence": []string{}}})
	progressOK(t, clear, 200)
	current = readWorking(t, a, base, token)
	if current.Revision != 3 || current.Progress.Summary != initialProgressSummary() || current.Progress.Next != "" || current.Progress.Blocker != "" || current.Progress.Evidence == nil || len(current.Progress.Evidence) != 0 {
		t.Fatal("partial clear lost summary or retained cleared values")
	}
	before := contextState(t, a)
	noop := a.call("PATCH", base, token, "noop", map[string]any{"expected_revision": 3, "progress": map[string]any{"next": ""}})
	progressOK(t, noop, 200)
	if !strings.Contains(noop.Body.String(), `"outcome":"unchanged"`) {
		t.Fatal("no-op gained a revision")
	}
	after := contextState(t, a)
	delete(before, "receipts")
	delete(after, "receipts")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("no-op wrote history or event")
	}
	progressOK(t, a.call("PATCH", base, token, "stale", map[string]any{"expected_revision": 2, "progress": map[string]any{"summary": "stale"}}), 409)
	progressOK(t, a.call("PATCH", base, token, "metadata", map[string]any{"expected_revision": 3, "title": "New title"}), 200)
	var count int
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM job_progress_history WHERE project_id=?`, p).Scan(&count); err != nil || count != 2 {
		t.Fatal("metadata edit duplicated progress history", count, err)
	}
	if err := progressSnapshot(t, a); err != nil {
		t.Fatal(err)
	}
	history := a.call("GET", base+"/progress/history?limit=1", token, "", nil)
	progressOK(t, history, 200)
	var page struct {
		Items []struct {
			JobRevision int64           `json:"job_revision"`
			Progress    workingProgress `json:"progress"`
		} `json:"items"`
		Cursor  string `json:"cursor"`
		HasMore bool   `json:"has_more"`
	}
	decodeResponse(t, history, &page)
	if len(page.Items) != 1 || page.Items[0].JobRevision != 3 || !page.HasMore || page.Cursor == "" {
		t.Fatal("progress history not newest first/bounded")
	}
	progressOK(t, a.call("GET", base+"/progress/history?cursor="+url.QueryEscape(page.Cursor), token, "", nil), 200)
}
func initialProgressSummary() string { return "  Current <literal> 雪\n" }

func TestUnifiedProgressStrictFieldsBoundsRollbackAndScope(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	for i, raw := range []string{`{"expected_revision":1,"progress":null}`, `{"expected_revision":1,"progress":{"summary":null}}`, `{"expected_revision":1,"progress":{"evidence":null}}`, `{"expected_revision":1,"progress":{"summary":"x","summary":"y"}}`, `{"expected_revision":1,"progress":{"unknown":"x"}}`, `{"expected_revision":null,"status":"active"}`, `{"expected_revision":1,"brief":null}`} {
		r := httptest.NewRequest("PATCH", base, strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", fmt.Sprint("bad-", i))
		w := httptest.NewRecorder()
		a.mux.ServeHTTP(w, r)
		progressOK(t, w, 400)
	}
	for i, patch := range []map[string]any{{"summary": strings.Repeat("x", 8193)}, {"next": strings.Repeat("x", 8193)}, {"blocker": strings.Repeat("x", 4097)}, {"evidence": []string{" "}}, {"evidence": []string{strings.Repeat("x", 1025)}}, {"summary": "x\x00"}, {"summary": strings.Repeat("x", 8192), "next": strings.Repeat("y", 8192), "blocker": "z"}} {
		progressOK(t, a.call("PATCH", base, token, fmt.Sprint("bounds-", i), map[string]any{"expected_revision": 1, "progress": patch}), 400)
	}
	other := a.createProject("other-working", "example/other-working")
	foreign := a.createWorker(other, "foreign")
	progressOK(t, a.call("PATCH", base, token, "foreign", map[string]any{"expected_revision": 1, "worker_id": foreign, "progress": map[string]any{"summary": "must rollback"}}), 404)
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_progress_history BEFORE INSERT ON job_progress_history BEGIN SELECT RAISE(ABORT,'synthetic failure');END`); err != nil {
		t.Fatal(err)
	}
	before := contextState(t, a)
	req := map[string]any{"expected_revision": 1, "status": "blocked", "worker_id": worker, "progress": map[string]any{"summary": "atomic"}}
	progressOK(t, a.call("PATCH", base, token, "retry-after-rollback", req), 500)
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("failure left a partial job/progress/history/event")
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_progress_history`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("PATCH", base, token, "retry-after-rollback", req), 200)
	readonly := a.grant(p, "working-readonly", "read")
	progressOK(t, a.call("PATCH", base, readonly, "readonly", map[string]any{"expected_revision": 2, "progress": map[string]any{"summary": "forbidden"}}), 403)
}

func TestUnifiedDefaultJobReadsExactPendingNotesChecklistAndNoArchives(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	id := strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/")
	progressOK(t, a.call("PUT", base+"/checklist", token, "checklist", progressReplace(1, 0, []progressItem{{ID: "done", Text: "Completed", Done: true}, {ID: "remaining", Text: "Continue", Done: false}})), 200)
	for i := 0; i < 7; i++ {
		r := a.call("POST", "/api/v1/projects/"+p+"/notes", token, fmt.Sprint("attached-", i), map[string]any{"worker_id": "", "phase": "", "job_id": id, "kind": "reminder", "body": fmt.Sprintf("Attached %d", i)})
		progressOK(t, r, 201)
	}
	progressOK(t, a.call("POST", "/api/v1/projects/"+p+"/notes", token, "worker-note", map[string]any{"job_id": "", "phase": "", "worker_id": worker, "kind": "reminder", "body": "unrelated worker reminder"}), 201)
	before := contextState(t, a)
	lookups := a.lookups
	got := readWorking(t, a, base, token)
	if got.Checklist.Total != 2 || got.Checklist.Completed != 1 || len(got.Checklist.Remaining) != 1 || got.Checklist.Remaining[0].ID != "remaining" || len(got.Notes.Items) != 5 || !got.Notes.HasMore || got.Notes.Cursor == "" {
		t.Fatal("default working context is incomplete or unbounded")
	}
	for _, note := range got.Notes.Items {
		if note.JobID != id {
			t.Fatal("unrelated worker reminder leaked")
		}
	}
	path := "/api/v1/projects/" + p + "/notes?job_id=" + id + "&status=pending&cursor=" + url.QueryEscape(got.Notes.Cursor)
	progressOK(t, a.call("GET", path, token, "", nil), 200)
	// Corrupt source archives and unrelated worker coordination; current reads
	// remain bounded to canonical progress, the checklist and exact-job notes.
	if _, err := a.store.DB().Exec(`INSERT INTO job_updates(id,project_id,job_id,job_revision,actor_id,worker_id,body,evidence_json,created_at) VALUES('forensic-update',?,?,1,'legacy','','PRIVATE archive','broken','broken')`, p, id); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{base, "/api/v1/projects/" + p + "/jobs", "/api/v1/projects/" + p + "/delivery"} {
		r := a.call("GET", route, token, "", nil)
		progressOK(t, r, 200)
		if bytes.Contains(r.Body.Bytes(), []byte("PRIVATE archive")) {
			t.Fatal("normal read expanded archive")
		}
	}
	// The old context and separate everyday mutation routes are absent.
	for _, route := range []struct{ method, suffix string }{{"GET", "/context"}, {"POST", "/updates"}, {"POST", "/checkpoints"}} {
		r := a.call(route.method, base+route.suffix, token, "removed", map[string]any{})
		if r.Code != 404 && r.Code != 405 {
			t.Fatal("retired route still registered", route, r.Code)
		}
	}
	after := contextState(t, a)
	delete(before, "job_updates")
	delete(after, "job_updates")
	if !reflect.DeepEqual(before, after) || lookups != a.lookups {
		t.Fatal("default read mutated state or contacted provider")
	}
}

func TestUnifiedProgressListsHaveBoundedUTF8PreviewsAndNoEvidence(t *testing.T) {
	a, p, token, _, base := progressFixture(t)
	summary := strings.Repeat("雪", 100)
	blocker := strings.Repeat("b", 241)
	progressOK(t, a.call("PATCH", base, token, "preview", map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": summary, "next": "Actual next", "blocker": blocker, "evidence": []string{"PRIVATE evidence"}}}), 200)
	for _, path := range []string{"/api/v1/projects/" + p + "/jobs", "/api/v1/projects/" + p + "/delivery"} {
		r := a.call("GET", path, token, "", nil)
		progressOK(t, r, 200)
		if bytes.Contains(r.Body.Bytes(), []byte("PRIVATE evidence")) || bytes.Contains(r.Body.Bytes(), []byte(summary)) || !utf8.Valid(r.Body.Bytes()) {
			t.Fatal("compact view leaked full progress or invalid UTF8")
		}
	}
	r := a.call("GET", "/api/v1/projects/"+p+"/jobs", token, "", nil)
	var page struct {
		Items []struct {
			ProgressSummary   string `json:"progress_summary"`
			Blocker           string `json:"blocker"`
			ProgressTruncated bool   `json:"progress_truncated"`
			BlockerTruncated  bool   `json:"blocker_truncated"`
		} `json:"items"`
	}
	decodeResponse(t, r, &page)
	if len(page.Items) != 1 || len(page.Items[0].ProgressSummary) > 240 || len(page.Items[0].Blocker) > 240 || !page.Items[0].ProgressTruncated || !page.Items[0].BlockerTruncated {
		t.Fatal("preview bounds/truncation absent")
	}
	if readWorking(t, a, base, token).Progress.Summary != summary {
		t.Fatal("default read shortened full progress")
	}
}

func TestUnifiedProgressConcurrentCASAndMalformedSnapshot(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- a.call("PATCH", base, token, fmt.Sprint("cas-", i), map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": fmt.Sprint(i)}}).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	success, conflict := 0, 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code == 409 {
			conflict++
		} else {
			t.Fatal("unexpected CAS status", code)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent revisions both succeeded")
	}
	if err := progressSnapshot(t, a); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB().Exec(`UPDATE job_progress SET evidence_json='null'`); err != nil {
		t.Fatal(err)
	}
	if err := progressSnapshot(t, a); err == nil {
		t.Fatal("malformed current snapshot accepted")
	}
	progressOK(t, a.call("GET", base, token, "", nil), 500)
}

func TestUnifiedChecklistGuardedReplacementRemainsIndependent(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	body := progressReplace(1, 0, []progressItem{{ID: "one", Text: "Step", Done: false}})
	first := a.call("PUT", base+"/checklist", token, "checklist-one", body)
	progressOK(t, first, 200)
	replay := a.call("PUT", base+"/checklist", token, "checklist-one", body)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("checklist replay changed")
	}
	if readWorking(t, a, base, token).Revision != 1 {
		t.Fatal("independent checklist changed job revision")
	}
	progressOK(t, a.call("PUT", base+"/checklist", token, "checklist-stale", body), 409)
}

func TestUnifiedDefaultReadCancellationDoesNotWrite(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	before := contextState(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest("GET", base, nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, r)
	if w.Code == 200 || !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("canceled read changed state")
	}
}

func TestUnifiedProgressProjectionSeesAtomicStateAndRollsBack(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	calls, fail := 0, true
	mux := http.NewServeMux()
	jobs.Register(mux, a.store, jobs.WithPublicProjection(func(ctx context.Context, tx *sql.Tx, project, id string) error {
		calls++
		var status, assigned, summary string
		if err := tx.QueryRowContext(ctx, `SELECT j.status,COALESCE(j.worker_id,''),p.summary FROM jobs j JOIN job_progress p ON p.project_id=j.project_id AND p.job_id=j.id WHERE j.project_id=? AND j.id=?`, project, id).Scan(&status, &assigned, &summary); err != nil {
			return err
		}
		if project != p || status != "blocked" || assigned != worker || summary != "Projection must see new progress" {
			t.Fatal("projection observed mixed pre/post state")
		}
		if fail {
			return errors.New("synthetic projection failure")
		}
		return nil
	}))
	a.mux = mux
	progressOK(t, a.call("PATCH", base, token, "private-progress", map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": "Private progress"}}), 200)
	if calls != 0 {
		t.Fatal("private progress-only change refreshed public projection")
	}
	req := map[string]any{"expected_revision": 2, "worker_id": worker, "status": "blocked", "progress": map[string]any{"summary": "Projection must see new progress"}}
	before := contextState(t, a)
	progressOK(t, a.call("PATCH", base, token, "projection-retry", req), 500)
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("projection failure left partial progress/history/job/receipt")
	}
	fail = false
	first := a.call("PATCH", base, token, "projection-retry", req)
	progressOK(t, first, 200)
	replay := a.call("PATCH", base, token, "projection-retry", req)
	if first.Body.String() != replay.Body.String() || calls != 2 {
		t.Fatal("projection replay repeated work")
	}
}

func TestUnifiedProgressSnapshotRejectsCurrentHistoryDamageWithoutArchiveFanout(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		archive   bool
	}{
		{"missing-current", `DELETE FROM job_progress`, false},
		{"current-future-revision", `UPDATE job_progress SET job_revision=99`, false},
		{"current-latest-disagreement", `UPDATE job_progress SET summary='different valid current'`, false},
		{"history-malformed-evidence", `UPDATE job_progress_history SET evidence_json='null'`, true},
		{"history-future-revision", `UPDATE job_progress_history SET job_revision=99`, true},
		{"history-invalid-actor", `UPDATE job_progress_history SET actor_id='invalid actor'`, true},
		{"history-missing-source", `DELETE FROM job_progress_history`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, token, _, base := progressFixture(t)
			progressOK(t, a.call("PATCH", base, token, "retained", map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": "retained current"}}), 200)
			if _, err := a.store.DB().Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			before := contextState(t, a)
			if err := progressSnapshot(t, a); err == nil {
				t.Fatal("corrupt current/history snapshot accepted")
			}
			if tc.archive {
				progressOK(t, a.call("GET", base, token, "", nil), 200)
			}
			if !reflect.DeepEqual(before, contextState(t, a)) {
				t.Fatal("snapshot validation or current read repaired corrupt archive")
			}
		})
	}
}

func TestUnifiedProgressAggregateBoundaryAndHistoryCursorScope(t *testing.T) {
	a, p, token, _, base := progressFixture(t)
	progressOK(t, a.call("PATCH", base, token, "aggregate", map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": strings.Repeat("s", 8192), "next": strings.Repeat("n", 8192), "evidence": []string{}}}), 200)
	before := contextState(t, a)
	progressOK(t, a.call("PATCH", base, token, "aggregate-overflow", map[string]any{"expected_revision": 2, "progress": map[string]any{"blocker": "x"}}), 400)
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("aggregate overflow partially applied")
	}
	progressOK(t, a.call("PATCH", base, token, "next-empty", map[string]any{"expected_revision": 2, "progress": map[string]any{"next": ""}}), 200)
	first := a.call("GET", base+"/progress/history?limit=1", token, "", nil)
	progressOK(t, first, 200)
	var page struct {
		Cursor string `json:"cursor"`
	}
	decodeResponse(t, first, &page)
	otherToken := a.grant(p, "other-progress-reader", "read")
	progressOK(t, a.call("GET", base+"/progress/history?cursor="+url.QueryEscape(page.Cursor), otherToken, "", nil), 400)
	other := a.createProject("isolated-progress", "example/isolated-progress")
	otherRead := a.grant(other, "foreign-progress-reader", "read")
	progressOK(t, a.call("GET", base+"/progress/history", otherRead, "", nil), 403)
}

func TestUnifiedDefaultJobReadIsOneSnapshotDuringAssignmentAndProgressChanges(t *testing.T) {
	a, p, token, worker, base := progressFixture(t)
	other := a.createWorker(p, "other-current")
	progressOK(t, a.call("PATCH", base, token, "initial-pair", map[string]any{"expected_revision": 1, "worker_id": worker, "progress": map[string]any{"summary": "A"}}), 200)
	done := make(chan error, 1)
	go func() {
		revision := int64(2)
		for i := 0; i < 30; i++ {
			assigned, summary := other, "B"
			if i%2 == 1 {
				assigned, summary = worker, "A"
			}
			r := a.call("PATCH", base, token, fmt.Sprint("paired-change-", i), map[string]any{"expected_revision": revision, "worker_id": assigned, "progress": map[string]any{"summary": summary}})
			if r.Code != 200 {
				done <- fmt.Errorf("paired mutation status %d", r.Code)
				return
			}
			revision++
		}
		done <- nil
	}()
	for i := 0; i < 40; i++ {
		got := readWorking(t, a, base, token)
		if !(got.WorkerID == worker && got.Progress.Summary == "A" || got.WorkerID == other && got.Progress.Summary == "B") {
			t.Fatal("read combined assignment/progress from different snapshots", got.WorkerID, got.Progress.Summary)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnifiedProgressMaximumCombinedEscapedPatchAndEncodedLimit(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	brief, summary, next := strings.Repeat("<", 32768), strings.Repeat("<", 8192), strings.Repeat("<", 8192)
	req := map[string]any{"expected_revision": 1, "brief": brief, "progress": map[string]any{"summary": summary, "next": next, "blocker": "", "evidence": []string{}}}
	encoded, err := json.Marshal(req)
	if err != nil || len(encoded) <= 256<<10 || len(encoded) > 384<<10 {
		t.Fatal("fixture did not exercise larger encoded PATCH bound", len(encoded), err)
	}
	progressOK(t, a.call("PATCH", base, token, "maximum-combined", req), 200)
	current := readWorking(t, a, base, token)
	if current.Brief != brief || current.Progress.Summary != summary || current.Progress.Next != next {
		t.Fatal("maximal combined PATCH shortened content")
	}
	before := contextState(t, a)
	oversized := strings.Repeat(" ", 384<<10) + `{"expected_revision":2,"status":"parked"}`
	r := httptest.NewRequest("PATCH", base, strings.NewReader(oversized))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", "encoded-overflow")
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, r)
	progressOK(t, w, 400)
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("above-limit encoded PATCH mutated state")
	}
}
