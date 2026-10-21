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
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/server"
)

type pauseValue struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	Paused   bool   `json:"paused"`
	Reason   string `json:"reason"`
}
type pauseEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	Paused        bool   `json:"paused"`
	Reason        string `json:"reason"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	CreatedAt     string `json:"created_at"`
}
type pausePage struct {
	Items   []pauseEntry `json:"items"`
	Cursor  string       `json:"cursor"`
	HasMore bool         `json:"has_more"`
}

func pausePath(project, worker string) string {
	return "/api/v1/projects/" + project + "/workers/" + worker + "/pause"
}
func pauseRequest(revision int64, paused bool, reason string) map[string]any {
	return map[string]any{"expected_revision": revision, "paused": paused, "reason": reason}
}
func pauseRead(t *testing.T, a *testAPI, path, token string) pauseValue {
	t.Helper()
	r := a.call("GET", path, token, "", nil)
	progressOK(t, r, 200)
	var value pauseValue
	decodeResponse(t, r, &value)
	return value
}
func pauseCounts(t *testing.T, a *testAPI) map[string]int {
	t.Helper()
	out := primaryCounts(t, a)
	for _, table := range []string{"worker_pause_state", "worker_pause_history"} {
		var n int
		if err := a.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestWorkerPauseIndependentLifecycleAndQuietReads(t *testing.T) {
	a, p, token, w, other, j1, _ := primaryFixture(t)
	path := pausePath(p, w)
	base := "/api/v1/projects/" + p
	for _, ensure := range []func(context.Context, *core.Store) error{inbox.EnsureSchema, reviewexec.EnsurePolicySchema} {
		if err := ensure(t.Context(), a.store); err != nil {
			t.Fatal(err)
		}
	}
	inbox.Register(a.mux, a.store)
	engine, err := reviewexec.New(a.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Register(a.mux)
	progressOK(t, a.callWorker("POST", base+"/messages", token, other, "private-message", map[string]any{"recipient_worker_id": w, "body": "Pending coordination"}), 201)
	progressOK(t, a.call("POST", base+"/reviews/policy", a.admin, "review-policy", map[string]any{"expected_revision": 0, "required_channels": []string{"hosted", "cli"}, "paused_channels": []string{}, "max_review_files": 0}), 200)
	progressOK(t, a.call("PUT", primaryPath(p, w), token, "focus", primaryRequest(0, j1)), 200)
	routes := []string{base + "/jobs/" + j1, base + "/jobs/" + j1 + "/history", base + "/jobs/" + j1 + "/checkpoints", base + "/jobs/" + j1 + "/updates", base + "/jobs/" + j1 + "/checklist", primaryPath(p, w), base + "/inbox/" + w, base + "/reviews/policy"}
	stable := map[string]string{}
	for _, route := range routes {
		r := a.call("GET", route, token, "", nil)
		progressOK(t, r, 200)
		stable[route] = r.Body.String()
	}
	before := pauseCounts(t, a)
	if current := pauseRead(t, a, path, token); current != (pauseValue{WorkerID: w}) {
		t.Fatal(current)
	}
	history := a.call("GET", path+"/history", token, "", nil)
	progressOK(t, history, 200)
	var empty pausePage
	decodeResponse(t, history, &empty)
	if empty.Items == nil || len(empty.Items) != 0 || !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("default read was not quiet", empty)
	}
	clear := a.call("PUT", path, token, "initial-resume", pauseRequest(0, false, ""))
	progressOK(t, clear, 200)
	var receipt receipt
	decodeResponse(t, clear, &receipt)
	if receipt.Revision != 0 || receipt.Outcome != "unchanged" {
		t.Fatal(receipt)
	}
	before["receipts"]++
	if !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("initial resume invented state/history")
	}
	reason := "  Wait <literal> 雪\n\tFor direction  "
	first := a.callWorker("PUT", path, token, other, "pause-original", pauseRequest(0, true, reason))
	progressOK(t, first, 200)
	decodeResponse(t, first, &receipt)
	if receipt.ID != w || receipt.Revision != 1 || receipt.Outcome != "updated" || strings.Contains(first.Body.String(), "direction") {
		t.Fatal("bad/noncompact pause receipt", receipt)
	}
	if got := pauseRead(t, a, path, token); got.Reason != reason || !got.Paused || got.Revision != 1 {
		t.Fatal("reason normalized", got)
	}
	before = pauseCounts(t, a)
	progressOK(t, a.call("PUT", path, token, "same-pause", pauseRequest(1, true, reason)), 200)
	before["receipts"]++
	if !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("identical pause changed state/event/history")
	}
	progressOK(t, a.call("PUT", path, token, "correct-reason", pauseRequest(1, true, "Corrected source")), 200)
	progressOK(t, a.call("PUT", path, token, "resume", pauseRequest(2, false, "")), 200)
	if got := pauseRead(t, a, path, token); got.Paused || got.Reason != "" || got.Revision != 3 {
		t.Fatal(got)
	}
	before = pauseCounts(t, a)
	replay := a.callWorker("PUT", path, token, other, "pause-original", pauseRequest(0, true, reason))
	progressOK(t, replay, 200)
	if replay.Body.String() != first.Body.String() || !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("historical replay repeated effects")
	}
	primaryError(t, a.callWorker("PUT", path, token, other, "pause-original", pauseRequest(0, true, "Changed request")), 409, "idempotency_conflict")
	for route, want := range stable {
		if a.call("GET", route, token, "", nil).Body.String() != want {
			t.Fatal("pause/resume changed unrelated coordination", route)
		}
	}
	// The pause is durable coordination state, not a write gate or review pause.
	progressOK(t, a.call("PUT", path, token, "paused-again", pauseRequest(3, true, "")), 200)
	progressOK(t, a.call("POST", base+"/jobs", token, "new-assignment", map[string]any{"title": "New assignment", "worker_id": w}), 201)
	progressOK(t, a.call("PATCH", base+"/jobs/"+j1, token, "blocked-while-paused", map[string]any{"expected_revision": 1, "status": "blocked"}), 200)
	progressOK(t, a.callWorker("PATCH", base+"/jobs/"+j1, token, w, "progress-while-paused", map[string]any{"expected_revision": 2, "progress": map[string]any{"summary": "Still coordinated", "next": "Wait"}}), 200)

	if got := pauseRead(t, a, path, token); !got.Paused || got.Revision != 4 || got.Reason != "" {
		t.Fatal("ordinary writes resumed worker", got)
	}
	for _, route := range []string{primaryPath(p, w), base + "/inbox/" + w, base + "/reviews/policy"} {
		if a.call("GET", route, token, "", nil).Body.String() != stable[route] {
			t.Fatal("paused writes changed unrelated state", route)
		}
	}
	r := a.call("GET", path+"/history", token, "", nil)
	progressOK(t, r, 200)
	var page pausePage
	decodeResponse(t, r, &page)
	if len(page.Items) != 4 || page.Items[3].Reason != reason || page.Items[3].ActorWorkerID != other || page.Items[3].ActorID == "" {
		t.Fatal(page)
	}
	rows, err := a.store.DB().Query(`SELECT payload_json FROM events WHERE event_type='worker.pause_changed'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if json.Unmarshal([]byte(body), &payload) != nil || len(payload) != 2 || payload["worker_id"] != w {
			t.Fatal("pause event leaked fields", body)
		}
		if _, ok := payload["paused"].(bool); !ok {
			t.Fatal(payload)
		}
		count++
	}
	if err = rows.Err(); err != nil || count != 4 || a.lookups != 1 {
		t.Fatal("events/provider work", count, err, a.lookups)
	}
}

func TestWorkerPauseValidationAuthorizationAndCursorBinding(t *testing.T) {
	a, p, token, w, other, _, _ := primaryFixture(t)
	path := pausePath(p, w)
	before := pauseCounts(t, a)
	zero := a.call("PUT", path, token, "stale-zero", pauseRequest(1, true, ""))
	primaryError(t, zero, 409, "worker_pause_revision_conflict")
	if !strings.Contains(zero.Body.String(), `"current_revision":0`) {
		t.Fatal(zero.Body.String())
	}
	invalid := []string{`{}`, `{"expected_revision":0,"paused":true}`, `{"expected_revision":0,"reason":""}`, `{"paused":true,"reason":""}`, `{"expected_revision":null,"paused":true,"reason":""}`, `{"expected_revision":0,"paused":null,"reason":""}`, `{"expected_revision":0,"paused":true,"reason":null}`, `{"expected_revision":-1,"paused":true,"reason":""}`, `{"expected_revision":9223372036854775807,"paused":true,"reason":""}`, `{"expected_revision":0.0,"paused":true,"reason":""}`, `{"expected_revision":0,"paused":false,"reason":"Not cleared"}`, `{"expected_revision":0,"paused":true,"reason":"","extra":true}`, `{"expected_revision":0,"Paused":true,"reason":""}`, `{"expected_revision":0,"paused":true,"reason":"","reason":""}`}
	for i, raw := range invalid {
		r := a.call("PUT", path, token, fmt.Sprint("invalid", i), json.RawMessage(raw))
		progressOK(t, r, 400)
	}
	for i, reason := range []string{"\x00", "\r", "\x7f", "\u0085", strings.Repeat("a", 1025), strings.Repeat("雪", 342)} {
		progressOK(t, a.call("PUT", path, token, fmt.Sprint("invalid-text", i), pauseRequest(0, true, reason)), 400)
	}
	if !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("invalid mutation changed state")
	}
	primaryError(t, a.call("GET", pausePath(p, "worker_missing"), token, "", nil), 404, "worker_not_found")
	primaryError(t, a.call("PUT", pausePath(p, "worker_missing"), token, "missing", pauseRequest(0, true, "")), 404, "worker_not_found")
	readonly := a.grant(p, "pause-readonly", "read")
	primaryError(t, a.call("PUT", path, readonly, "readonly", pauseRequest(0, true, "")), 403, "forbidden")
	progressOK(t, a.call("GET", path, readonly, "", nil), 200)
	p2 := a.createProject("pause-other", "synthetic/pause-other")
	foreign := a.grant(p2, "pause-foreign", "read", "write")
	fw := a.createWorker(p2, "Builder")
	primaryError(t, a.call("GET", path, foreign, "", nil), 403, "forbidden")
	primaryError(t, a.call("PUT", path, foreign, "cross", pauseRequest(0, true, "")), 403, "forbidden")
	primaryError(t, a.call("GET", pausePath(p, fw), token, "", nil), 404, "worker_not_found")
	// Default paging is one page; explicit continuation is revision based even
	// when a new change arrives after the first page.
	for i := int64(0); i < 21; i++ {
		progressOK(t, a.callWorker("PUT", path, token, other, fmt.Sprint("revision", i), pauseRequest(i, true, fmt.Sprint("Reason ", i))), 200)
	}
	response := a.call("GET", path+"/history", token, "", nil)
	progressOK(t, response, 200)
	var page pausePage
	decodeResponse(t, response, &page)
	if len(page.Items) != 20 || page.Items[0].Revision != 21 || page.Items[19].Revision != 2 || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	progressOK(t, a.call("PUT", path, token, "newer", pauseRequest(21, false, "")), 200)
	cursor := "?cursor=" + url.QueryEscape(page.Cursor)
	response = a.call("GET", path+"/history"+cursor, token, "", nil)
	progressOK(t, response, 200)
	var rest pausePage
	decodeResponse(t, response, &rest)
	if len(rest.Items) != 1 || rest.Items[0].Revision != 1 || rest.HasMore || rest.Items[0].ActorWorkerID != other {
		t.Fatal(rest)
	}
	for _, test := range []struct{ route, token string }{{pausePath(p, other) + "/history" + cursor, token}, {pausePath(p2, fw) + "/history" + cursor, foreign}, {path + "/history" + cursor, a.admin}, {primaryPath(p, w) + "/history" + cursor, token}, {path + "/history?limit=101", token}, {path + "/history?limit=0", token}, {path + "/history?limit=1&limit=2", token}, {path + "/history?cursor=%zz", token}, {path + "?limit=1", token}} {
		progressOK(t, a.call("GET", test.route, test.token, "", nil), 400)
	}
	// The maximum valid reason is accepted literally, even when JSON escaping
	// makes the current response larger than the primary focus response budget.
	reason := strings.Repeat("<", 1024)
	progressOK(t, a.call("PUT", path, token, "maximum-text", pauseRequest(22, true, reason)), 200)
	if got := pauseRead(t, a, path, token); got.Reason != reason || got.Revision != 23 {
		t.Fatal("maximum reason was truncated")
	}
	sum := sha256.Sum256([]byte(token))
	var credential string
	if err := a.store.DB().QueryRow(`SELECT id FROM credentials WHERE token_sha256=?`, fmt.Sprintf("%x", sum)).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("DELETE", "/api/v1/projects/"+p+"/credentials/"+credential, a.admin, "revoke", nil), 200)
	primaryError(t, a.callWorker("PUT", path, token, other, "revision0", pauseRequest(0, true, "Reason 0")), 401, "unauthorized")
}

func TestWorkerPauseRacesAtomicRollbackAndRevisionBounds(t *testing.T) {
	a, p, token, w, _, _, _ := primaryFixture(t)
	path := pausePath(p, w)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			responses <- a.call("PUT", path, token, fmt.Sprint("race", i), pauseRequest(0, true, fmt.Sprint("Reason", i)))
		}(i)
	}
	close(start)
	wg.Wait()
	close(responses)
	successes, conflicts := 0, 0
	for r := range responses {
		if r.Code == 200 {
			successes++
		} else {
			primaryError(t, r, 409, "worker_pause_revision_conflict")
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	selected := pauseRead(t, a, path, token)
	before := pauseCounts(t, a)
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_pause BEFORE INSERT ON events WHEN NEW.event_type='worker.pause_changed' BEGIN SELECT RAISE(ABORT,'owned fixture failure');END`); err != nil {
		t.Fatal(err)
	}
	failed := a.call("PUT", path, token, "fault-key", pauseRequest(1, false, ""))
	if failed.Code < 500 {
		t.Fatal(failed.Code)
	}
	if !reflect.DeepEqual(before, pauseCounts(t, a)) || pauseRead(t, a, path, token) != selected {
		t.Fatal("event fault left staged pause/history/receipt")
	}
	if _, err := a.store.DB().Exec(`DROP TRIGGER fail_pause`); err != nil {
		t.Fatal(err)
	}
	progressOK(t, a.call("PUT", path, token, "fault-key", pauseRequest(1, false, "")), 200)
	if _, err := a.store.DB().Exec(`UPDATE worker_pause_state SET revision=? WHERE project_id=? AND worker_id=?`, int64(math.MaxInt64-1), p, w); err != nil {
		t.Fatal(err)
	}
	original := a.call("PUT", path, token, "last-revision", pauseRequest(math.MaxInt64-1, true, "Last revision"))
	progressOK(t, original, 200)
	if value := pauseRead(t, a, path, token); value.Revision != math.MaxInt64 {
		t.Fatal(value)
	}
	before = pauseCounts(t, a)
	primaryError(t, a.call("PUT", path, token, "overflow", pauseRequest(math.MaxInt64, true, "Last revision")), 400, "invalid_revision")
	replay := a.call("PUT", path, token, "last-revision", pauseRequest(math.MaxInt64-1, true, "Last revision"))
	progressOK(t, replay, 200)
	if replay.Body.String() != original.Body.String() || !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("overflow/replay altered durable state")
	}
}

func TestWorkerPauseJobsFourMigrationAndRestart(t *testing.T) {
	a, p, token, w, _, j1, _ := primaryFixture(t)
	path := pausePath(p, w)
	progressOK(t, a.call("PUT", primaryPath(p, w), token, "retained-focus", primaryRequest(0, j1)), 200)
	if _, err := a.store.DB().Exec(`DROP TABLE job_progress_history; DROP TABLE job_progress; DROP TABLE job_summary_history; DROP TABLE job_summaries; DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history;DROP TABLE worker_pause_state;UPDATE metadata SET value='4' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	stable := primaryCounts(t, a)
	focus := a.call("GET", primaryPath(p, w), token, "", nil).Body.String()
	var dbfile string
	if err := a.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbfile); err != nil {
		t.Fatal(err)
	}
	reopen := func() {
		if err := a.store.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := core.Open(t.Context(), filepath.Dir(dbfile))
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
	if !reflect.DeepEqual(stable, primaryCounts(t, a)) || a.call("GET", primaryPath(p, w), token, "", nil).Body.String() != focus {
		t.Fatal("migration changed existing focus/jobs/receipt/event")
	}
	if value := pauseRead(t, a, path, token); value != (pauseValue{WorkerID: w}) {
		t.Fatal("migration synthesized pause", value)
	}
	original := a.call("PUT", path, token, "restart-pause", pauseRequest(0, true, "Retained pause"))
	progressOK(t, original, 200)
	expected := pauseRead(t, a, path, token)
	history := a.call("GET", path+"/history", token, "", nil).Body.String()
	before := pauseCounts(t, a)
	reopen()
	if pauseRead(t, a, path, token) != expected || a.call("GET", path+"/history", token, "", nil).Body.String() != history {
		t.Fatal("restart changed pause")
	}
	replay := a.call("PUT", path, token, "restart-pause", pauseRequest(0, true, "Retained pause"))
	progressOK(t, replay, 200)
	if replay.Body.String() != original.Body.String() || !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("restart replay changed effects")
	}
}
