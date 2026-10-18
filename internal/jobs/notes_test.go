package jobs_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

type noteValue struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	Phase         string `json:"phase"`
	Kind          string `json:"kind"`
	Body          string `json:"body"`
	Status        string `json:"status"`
	StatusReason  string `json:"status_reason"`
	Revision      int64  `json:"revision"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	RecordedAt    string `json:"recorded_at"`
	BodyPreview   string `json:"body_preview"`
	BodyTruncated bool   `json:"body_truncated"`
}
type notesPageValue struct {
	Items   []noteValue `json:"items"`
	Cursor  string      `json:"cursor"`
	HasMore bool        `json:"has_more"`
}

func noteCall(a *testAPI, method, path, token, actor, key string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
	}
	return noteRaw(a, method, path, token, actor, key, raw)
}
func noteRaw(a *testAPI, method, path, token, actor, key string, raw []byte) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", a.store.InstanceID())
	r.Header.Set("Content-Type", "application/json")
	if actor != "" {
		r.Header.Set("X-FC-Worker", actor)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, r)
	return w
}
func noteOK(t *testing.T, r *httptest.ResponseRecorder, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("note status%d want%d: %s", r.Code, status, r.Body.String())
	}
}
func noteError(t *testing.T, r *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	noteOK(t, r, status)
	if !strings.Contains(r.Body.String(), `"code":"`+code+`"`) {
		t.Fatal(r.Body.String())
	}
}
func noteRead(t *testing.T, a *testAPI, path, token string) noteValue {
	t.Helper()
	w := noteCall(a, "GET", path, token, "", "", nil)
	noteOK(t, w, 200)
	var n noteValue
	decodeResponse(t, w, &n)
	return n
}
func noteList(t *testing.T, a *testAPI, path, token string) notesPageValue {
	t.Helper()
	w := noteCall(a, "GET", path, token, "", "", nil)
	noteOK(t, w, 200)
	var p notesPageValue
	decodeResponse(t, w, &p)
	if p.Items == nil {
		t.Fatal("null page")
	}
	return p
}
func noteCounts(t *testing.T, a *testAPI) string {
	t.Helper()
	var values []string
	for _, table := range []string{"notes", "note_revisions", "receipts", "events"} {
		var n int
		if err := a.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		values = append(values, fmt.Sprint(n))
	}
	return strings.Join(values, "/")
}
func noteCreate(t *testing.T, a *testAPI, base, token, actor, key, worker, job, phase, body string) receipt {
	t.Helper()
	r := noteCall(a, "POST", base, token, actor, key, map[string]any{"worker_id": worker, "job_id": job, "phase": phase, "kind": "reminder", "body": body})
	noteOK(t, r, 201)
	var v receipt
	decodeResponse(t, r, &v)
	if v.Revision != 1 || v.Outcome != "created" {
		t.Fatal(v)
	}
	return v
}

func TestNotesLifecycleExactTextHistoryReplayAndAtomicity(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("notes", "owner/notes")
	token := a.grant(project, "notes-writer-token", "read", "write")
	reader := a.grant(project, "notes-reader-token", "read")
	actor := a.createWorker(project, "actor")
	target := a.createWorker(project, "target")
	base := "/api/v1/projects/" + project + "/notes"
	body := strings.Repeat("<", 239) + "雪\n\t exact private body  "
	created := noteCreate(t, a, base, token, actor, "create", target, "", "", body)
	path := base + "/" + created.ID
	initial := noteRead(t, a, path, reader)
	if initial.Body != body || initial.Status != "pending" || initial.StatusReason != "" || initial.WorkerID != target || initial.ProjectID != project {
		t.Fatal(initial)
	}
	patch := func(key string, req map[string]any) receipt {
		t.Helper()
		r := noteCall(a, "PATCH", path, token, actor, key, req)
		noteOK(t, r, 200)
		var out receipt
		decodeResponse(t, r, &out)
		return out
	}
	dismissed := patch("dismiss", map[string]any{"expected_revision": 1, "status": "dismissed", "status_reason": "  superseded\twith reason\n  "})
	if dismissed.Revision != 2 {
		t.Fatal(dismissed)
	}
	correction := patch("body-correction", map[string]any{"expected_revision": 2, "body": "Replacement <exact>\n"})
	if correction.Revision != 3 {
		t.Fatal(correction)
	}
	current := noteRead(t, a, path, reader)
	if current.Status != "dismissed" || current.StatusReason != "  superseded\twith reason\n  " {
		t.Fatal("body-only patch lost dismissal", current)
	}
	before := noteCounts(t, a)
	same := patch("no-change", map[string]any{"expected_revision": 3, "body": current.Body})
	if same.Outcome != "unchanged" || same.Revision != 3 || noteRead(t, a, path, reader) != current {
		t.Fatal(same)
	}
	priorCounts, afterCounts := strings.Split(before, "/"), strings.Split(noteCounts(t, a), "/")
	if priorCounts[0] != afterCounts[0] || priorCounts[1] != afterCounts[1] || priorCounts[3] != afterCounts[3] || priorCounts[2] == afterCounts[2] {
		t.Fatal("no-op changed notes/history/events or lost its receipt")
	}
	reopened := patch("reopen", map[string]any{"expected_revision": 3, "status": "pending"})
	if reopened.Revision != 4 || noteRead(t, a, path, reader).StatusReason != "" {
		t.Fatal("reopening did not clear reason")
	}
	patch("consume", map[string]any{"expected_revision": 4, "status": "consumed"})
	replay := patch("dismiss", map[string]any{"expected_revision": 1, "status": "dismissed", "status_reason": "  superseded\twith reason\n  "})
	if replay != dismissed {
		t.Fatal("original patch receipt changed", replay)
	}
	replayCreate := noteCreate(t, a, base, token, actor, "create", target, "", "", body)
	if replayCreate != created {
		t.Fatal("creation replay changed after edits")
	}
	noteError(t, noteCall(a, "PATCH", path, token, actor, "dismiss", map[string]any{"expected_revision": 1, "body": "other"}), 409, "idempotency_conflict")
	noteError(t, noteCall(a, "PATCH", path, token, actor, "stale-no-op", map[string]any{"expected_revision": 1, "status": "consumed"}), 409, "note_revision_conflict")
	first := noteRead(t, a, path+"/history/1", reader)
	if first.Body != body || first.Status != "pending" || first.ActorWorkerID != actor || first.ActorID == "" || first.RecordedAt != first.UpdatedAt {
		t.Fatal("first immutable flat revision changed", first)
	}
	if old := noteRead(t, a, path+"/history/2", reader); old.StatusReason != "  superseded\twith reason\n  " || old.Body != body {
		t.Fatal(old)
	}
	archive := noteList(t, a, path+"/history?limit=2", reader)
	if len(archive.Items) != 2 || archive.Items[0].Revision != 5 || archive.Items[1].Revision != 4 || !archive.HasMore {
		t.Fatal(archive)
	}
	older := noteList(t, a, path+"/history?limit=2&cursor="+url.QueryEscape(archive.Cursor), reader)
	if older.Items[0].Revision != 3 {
		t.Fatal(older)
	}
	originalSummary := noteList(t, a, path+"/history?limit=100", reader).Items[4]
	if originalSummary.BodyPreview != strings.Repeat("<", 239) || !originalSummary.BodyTruncated || !utf8.ValidString(originalSummary.BodyPreview) {
		t.Fatal("preview split a rune or replaced authoritative text", originalSummary)
	}
	raw := noteCall(a, "GET", path+"/history", reader, "", "", nil).Body.String()
	for _, field := range []string{`"body":`, `"status_reason":`, `"state":`} {
		if strings.Contains(raw, field) {
			t.Fatal("compact history expanded private content", field)
		}
	}
	readBefore := noteCounts(t, a)
	lookups := a.lookups
	noteRead(t, a, path, reader)
	noteList(t, a, base+"?status=all", reader)
	noteRead(t, a, path+"/history/2", reader)
	if noteCounts(t, a) != readBefore || a.lookups != lookups {
		t.Fatal("reads wrote state or queried provider")
	}
	// Event insertion failure must roll back current state, history and receipt.
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_note_event BEFORE INSERT ON events WHEN NEW.event_type='note.updated' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	current = noteRead(t, a, path, reader)
	before = noteCounts(t, a)
	failed := noteCall(a, "PATCH", path, token, actor, "atomic", map[string]any{"expected_revision": 5, "body": "Atomic change"})
	if failed.Code < 500 || noteCounts(t, a) != before || noteRead(t, a, path, reader) != current {
		t.Fatal("failed mutation partially committed", failed.Code, failed.Body.String())
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_note_event`); err != nil {
		t.Fatal(err)
	}
	if v := patch("atomic", map[string]any{"expected_revision": 5, "body": "Atomic change"}); v.Revision != 6 {
		t.Fatal(v)
	}
	var leaked int
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type LIKE 'note.%' AND (payload_json LIKE '%private%' OR payload_json LIKE '%reason%' OR payload_json LIKE '%Replacement%')`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("event leaked note text", err)
	}
}

func TestNotesFiltersAssignmentExpansionAndScopeIsolation(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("notes-filter", "owner/filters")
	other := a.createProject("notes-other", "owner/other")
	token := a.grant(p, "notes-filter-token", "read", "write")
	otherToken := a.grant(other, "notes-other-token", "read", "write")
	w1, w2 := a.createWorker(p, "one"), a.createWorker(p, "two")
	foreign := a.createWorker(other, "foreign")
	base := "/api/v1/projects/" + p + "/notes"
	response := a.call("POST", "/api/v1/projects/"+p+"/jobs", token, "job", map[string]any{"title": "Retained job", "brief": "Do not expand", "worker_id": w1})
	noteOK(t, response, 201)
	var job receipt
	decodeResponse(t, response, &job)
	direct := noteCreate(t, a, base, token, w2, "direct", w1, "", "plan", "direct")
	attached := noteCreate(t, a, base, token, w1, "attached", w2, job.ID, "plan", "job-related")
	phase := noteCreate(t, a, base, token, w1, "phase", "", "", "other", "phase only")
	noteOK(t, noteCall(a, "PATCH", base+"/"+phase.ID, token, w1, "consume", map[string]any{"expected_revision": 1, "status": "consumed"}), 200)
	if page := noteList(t, a, base, token); len(page.Items) != 2 {
		t.Fatal("default did not select pending", page)
	}
	query := base + "?worker_id=" + w1 + "&include_assigned_jobs=true"
	if page := noteList(t, a, query, token); len(page.Items) != 2 {
		t.Fatal("assignment expansion did not OR worker/job", page)
	}
	if page := noteList(t, a, base+"?worker_id="+w1, token); len(page.Items) != 1 || page.Items[0].ID != direct.ID {
		t.Fatal(page)
	}
	if page := noteList(t, a, query+"&job_id="+job.ID+"&phase=plan&kind=reminder", token); len(page.Items) != 1 || page.Items[0].ID != attached.ID {
		t.Fatal("filters did not combine with AND", page)
	}
	noteOK(t, a.call("PATCH", "/api/v1/projects/"+p+"/jobs/"+job.ID, token, "transfer", map[string]any{"expected_revision": 1, "worker_id": w2}), 200)
	if page := noteList(t, a, query, token); len(page.Items) != 1 || page.Items[0].ID != direct.ID {
		t.Fatal("assignment selection stale after transfer", page)
	}
	if page := noteList(t, a, base+"?worker_id="+w2+"&include_assigned_jobs=true", token); len(page.Items) != 1 || page.Items[0].ID != attached.ID {
		t.Fatal(page)
	}
	if n := noteRead(t, a, base+"/"+attached.ID, token); n.Revision != 1 || n.WorkerID != w2 || n.JobID != job.ID {
		t.Fatal("job transfer mutated note", n)
	}
	if page := noteList(t, a, base+"?status=all&limit=1", token); !page.HasMore || len(page.Items) != 1 {
		t.Fatal(page)
	} else {
		noteError(t, noteCall(a, "GET", base+"?status=pending&cursor="+url.QueryEscape(page.Cursor), token, "", "", nil), 400, "validation_failed")
		noteError(t, noteCall(a, "GET", base+"/"+direct.ID+"/history?cursor="+url.QueryEscape(page.Cursor), token, "", "", nil), 400, "validation_failed")
	}
	for _, q := range []string{"worker_id=", "job_id=", "phase=", "kind=", "status=", "status=no", "include_assigned_jobs=true", "include_assigned_jobs=1", "worker_id=" + w1 + "&worker_id=" + w2, "unknown=x"} {
		noteError(t, noteCall(a, "GET", base+"?"+q, token, "", "", nil), 400, "invalid_query")
	}
	if page := noteList(t, a, base+"?worker_id=worker-missing&include_assigned_jobs=true", token); len(page.Items) != 0 {
		t.Fatal(page)
	}
	noteError(t, noteCall(a, "GET", base+"/"+direct.ID, otherToken, "", "", nil), 403, "forbidden")
	noteError(t, noteCall(a, "GET", "/api/v1/projects/"+other+"/notes/"+direct.ID, otherToken, "", "", nil), 404, "note_not_found")
	noteError(t, noteCall(a, "POST", base, token, w1, "foreign", map[string]any{"worker_id": foreign, "job_id": "", "phase": "", "kind": "source", "body": "Foreign selector"}), 404, "worker_not_found")
}

func TestNotesStrictArgumentsConcurrentGuardsAndStoredCorruption(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("strict-notes", "owner/strict")
	token := a.grant(p, "strict-notes-token", "read", "write")
	reader := a.grant(p, "strict-reader-token", "read")
	worker := a.createWorker(p, "one")
	base := "/api/v1/projects/" + p + "/notes"
	valid := fmt.Sprintf(`{"worker_id":%q,"job_id":"","phase":"","kind":"reminder","body":"valid"}`, worker)
	for _, raw := range []string{`{}`, strings.Replace(valid, `"body":"valid"`, `"body":null`, 1), strings.Replace(valid, `"body":"valid"`, `"body":"\u0085"`, 1), strings.Replace(valid, `"body":"valid"`, `"body":"\r"`, 1), strings.Replace(valid, `"body":"valid"`, `"body":"\ud800"`, 1), strings.Replace(valid, `"body":"valid"`, `"body":"valid","body":"again"`, 1), strings.Replace(valid, `"body":"valid"`, `"body":"valid","unknown":true`, 1), strings.Replace(valid, `"body":"valid"`, `"Body":"valid"`, 1), strings.Replace(valid, worker, "", 1)} {
		before := noteCounts(t, a)
		noteError(t, noteRaw(a, "POST", base, token, worker, "invalid", []byte(raw)), 400, "validation_failed")
		if noteCounts(t, a) != before {
			t.Fatal("invalid request wrote effects")
		}
	}
	created := noteCreate(t, a, base, token, worker, "good", worker, "", "", "valid")
	path := base + "/" + created.ID
	for _, request := range []map[string]any{{"body": "missing guard"}, {"expected_revision": 0, "body": "x"}, {"expected_revision": int64(math.MaxInt64), "body": "x"}, {"expected_revision": 1}, {"expected_revision": 1, "body": nil}, {"expected_revision": 1, "status": "dismissed"}, {"expected_revision": 1, "status": "consumed", "status_reason": "not empty"}, {"expected_revision": 1, "worker_id": ""}, {"expected_revision": 1, "body": strings.Repeat("x", (16<<10)+1)}, {"expected_revision": 1, "phase": strings.Repeat("p", 201)}} {
		noteError(t, noteCall(a, "PATCH", path, token, worker, "invalid-patch", request), 400, "validation_failed")
	}
	noteError(t, noteCall(a, "PATCH", path, reader, "", "readonly", map[string]any{"expected_revision": 1, "body": "denied"}), 403, "forbidden")
	statuses := make(chan int, 2)
	var wait sync.WaitGroup
	for i := range 2 {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			statuses <- noteCall(a, "PATCH", path, token, worker, fmt.Sprint("race-", i), map[string]any{"expected_revision": 1, "body": fmt.Sprint("winner-", i)}).Code
		}(i)
	}
	wait.Wait()
	close(statuses)
	ok, conflicts := 0, 0
	for status := range statuses {
		if status == 200 {
			ok++
		} else if status == 409 {
			conflicts++
		} else {
			t.Fatal(status)
		}
	}
	if ok != 1 || conflicts != 1 || noteRead(t, a, path, reader).Revision != 2 {
		t.Fatal("CAS did not select one update")
	}
	before := noteCounts(t, a)
	if _, err := a.store.DB().Exec(`UPDATE notes SET body='' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	noteError(t, noteCall(a, "GET", path, reader, "", "", nil), 500, "storage_unavailable")
	if noteCounts(t, a) != before {
		t.Fatal("stored corruption read wrote state")
	}
}

func TestNotesRestartOriginalReceiptAndCurrentAuthorization(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("notes-restart", "owner/restart")
	token := a.grant(p, "notes-restart-token", "read", "write")
	worker := a.createWorker(p, "worker")
	base := "/api/v1/projects/" + p + "/notes"
	created := noteCreate(t, a, base, token, worker, "create-original", worker, "", "", "Original private body")
	path := base + "/" + created.ID
	request := map[string]any{"expected_revision": 1, "body": "First change"}
	first := noteCall(a, "PATCH", path, token, worker, "patch-original", request)
	noteOK(t, first, 200)
	noteOK(t, noteCall(a, "PATCH", path, token, worker, "later", map[string]any{"expected_revision": 2, "status": "consumed"}), 200)
	originalCurrent := noteRead(t, a, path, token)
	originalHistory := noteCall(a, "GET", path+"/history?limit=100", token, "", "", nil).Body.String()
	before := noteCounts(t, a)
	var databaseFile string
	if err := a.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&databaseFile); err != nil {
		t.Fatal(err)
	}
	instance := a.store.InstanceID()
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(databaseFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err = jobs.EnsureSchema(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	a.mux = http.NewServeMux()
	a.mux.Handle("/", server.New(reopened))
	jobs.Register(a.mux, reopened)
	replay := noteCall(a, "PATCH", path, token, worker, "patch-original", request)
	noteOK(t, replay, 200)
	if replay.Body.String() != first.Body.String() || noteRead(t, a, path, token) != originalCurrent || noteCall(a, "GET", path+"/history?limit=100", token, "", "", nil).Body.String() != originalHistory || noteCounts(t, a) != before || a.store.InstanceID() != instance {
		t.Fatal("restart replay rewrote original receipt/history")
	}
	if _, err = a.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id IN (SELECT credential_id FROM grants WHERE project_id=?)`, p); err != nil {
		t.Fatal(err)
	}
	revoked := noteCall(a, "PATCH", path, token, worker, "patch-original", request)
	if revoked.Code != 401 && revoked.Code != 403 {
		t.Fatal("revocation replay returned retained access", revoked.Code)
	}
	if noteCounts(t, a) != before {
		t.Fatal("revoked replay wrote effects")
	}
}

func TestNotesCurrentReadsDoNotExpandArchiveAndClockIsStructural(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("notes-local-read", "owner/local")
	token := a.grant(p, "notes-local-token", "read", "write")
	worker := a.createWorker(p, "worker")
	base := "/api/v1/projects/" + p + "/notes"
	created := noteCreate(t, a, base, token, worker, "create", worker, "", "", "Original")
	path := base + "/" + created.ID
	noteOK(t, noteCall(a, "PATCH", path, token, worker, "change", map[string]any{"expected_revision": 1, "body": "Changed"}), 200)
	earlier := "2000-01-01T00:00:00.100000000Z"
	if _, err := a.store.DB().Exec(`UPDATE notes SET updated_at=? WHERE id=?`, earlier, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB().Exec(`UPDATE note_revisions SET updated_at=?,recorded_at=? WHERE note_id=? AND revision=2`, earlier, earlier, created.ID); err != nil {
		t.Fatal(err)
	}
	if n := noteRead(t, a, path, token); n.UpdatedAt != earlier {
		t.Fatal("valid rollback timestamp rejected")
	}
	if n := noteRead(t, a, path+"/history/2", token); n.RecordedAt != earlier {
		t.Fatal("history inferred wall chronology")
	}
	// Current reads own only current state; an unavailable archive is not an
	// implicit read dependency. Operator backup validates full history separately.
	if _, err := a.store.DB().Exec(`DROP TABLE note_revisions`); err != nil {
		t.Fatal(err)
	}
	before := noteCountsWithoutArchive(t, a)
	noteRead(t, a, path, token)
	noteList(t, a, base, token)
	if noteCountsWithoutArchive(t, a) != before {
		t.Fatal("current reads mutated coordination")
	}
}
func noteCountsWithoutArchive(t *testing.T, a *testAPI) string {
	t.Helper()
	var events, receipts int
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(events, "/", receipts)
}
