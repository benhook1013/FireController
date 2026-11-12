package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
)

type retirementPreview struct {
	WorkerID  string `json:"worker_id"`
	Retired   bool   `json:"retired"`
	CanRetire bool   `json:"can_retire"`
	Blockers  struct {
		Jobs, Notes, Messages struct {
			Count int      `json:"count"`
			IDs   []string `json:"ids"`
		}
	} `json:"blockers"`
}

func retirementFixture(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	f := setup(t)
	for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema} {
		if err := ensure(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	jobs.Register(mux, f.store)
	inbox.Register(mux, f.store)
	f.handler = mux
	project := f.project(t, "owner/one")
	return f, project, f.worker(t, project, "Actor"), f.worker(t, project, "Retiring")
}

func retirementJSON(t *testing.T, f *fixture, method, path, token, actor string, body any, status int) map[string]any {
	t.Helper()
	text := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		text = string(data)
	}
	return require(t, request(f.handler, method, path, token, core.NewID("test"), actor, text), status)
}

func retirementStatus(t *testing.T, f *fixture, path, token string) retirementPreview {
	t.Helper()
	w := request(f.handler, "GET", path, token, "", "", "")
	require(t, w, 200)
	var result retirementPreview
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkerRetirementBlockersCleanupHistoryAndActiveTargets(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	base := "/api/v1/projects/" + project
	path := base + "/workers/" + target
	writer, manager := "retirement-writer", "retirement-manager"
	f.grant(t, project, writer, true)
	projectManagerGrant(t, f, project, manager)
	replacement := f.worker(t, project, "Replacement")
	job := retirementJSON(t, f, "POST", base+"/jobs", writer, actor, map[string]any{"title": "Unfinished", "worker_id": target}, 201)["id"].(string)
	completed := retirementJSON(t, f, "POST", base+"/jobs", writer, actor, map[string]any{"title": "Historical", "worker_id": target, "status": "completed"}, 201)["id"].(string)
	createNote := func(worker, job string) string {
		return retirementJSON(t, f, "POST", base+"/notes", writer, actor, map[string]any{"worker_id": worker, "job_id": job, "phase": "", "kind": "reminder", "body": "Retained reminder"}, 201)["id"].(string)
	}
	direct, assigned, historical := createNote(target, ""), createNote("", job), createNote("", completed)
	message := retirementJSON(t, f, "POST", base+"/messages", writer, actor, map[string]any{"recipient_worker_id": target, "body": "Retained incoming message"}, 201)["id"].(string)
	preview := retirementStatus(t, f, path+"/retirement", writer)
	if preview.CanRetire || preview.Retired || preview.WorkerID != target || preview.Blockers.Jobs.Count != 1 || preview.Blockers.Notes.Count != 3 || preview.Blockers.Messages.Count != 1 || preview.Blockers.Jobs.IDs[0] != job || preview.Blockers.Messages.IDs[0] != message {
		t.Fatal("unfinished applicable work was omitted", preview, assigned)
	}
	before := count(t, f.store, "events")
	blocked := require(t, request(f.handler, "POST", path+"/retire", writer, "retire-target", actor, `{}`), 409)
	if blocked["error"].(map[string]any)["code"] != "worker_retirement_blocked" || count(t, f.store, "events") != before || count(t, f.store, "worker_retirements") != 0 {
		t.Fatal("blocked retirement changed state", blocked)
	}
	retirementJSON(t, f, "PATCH", base+"/jobs/"+job, writer, actor, map[string]any{"expected_revision": 1, "worker_id": replacement}, 200)
	preview = retirementStatus(t, f, path+"/retirement", writer)
	if preview.Blockers.Jobs.Count != 0 || preview.Blockers.Notes.Count != 2 {
		t.Fatal("job reassignment did not move its pending note applicability", preview)
	}
	for _, id := range []string{direct, historical} {
		retirementJSON(t, f, "PATCH", base+"/notes/"+id, writer, actor, map[string]any{"expected_revision": 1, "status": "consumed"}, 200)
	}
	ack := base + "/messages/" + message + "/ack"
	body := `{"worker_id":"` + target + `"}`
	require(t, request(f.handler, "POST", ack, writer, "not-manager", actor, body), 403)
	require(t, request(f.handler, "POST", ack, manager, "manager-ack", actor, body), 200)
	var principal, payload string
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE project_id=? AND event_type='message.acknowledged' AND resource_id=?`, project, message).Scan(&principal, &payload); err != nil || principal == "" || !strings.Contains(payload, target) || !strings.Contains(payload, `"actor_worker_id":"`+actor+`"`) {
		t.Fatal("on-behalf acknowledgement lost actual actor or recipient", principal, payload, err)
	}
	preview = retirementStatus(t, f, path+"/retirement", writer)
	if !preview.CanRetire || preview.Blockers.Messages.Count != 0 || preview.Blockers.Notes.Count != 0 {
		t.Fatal("explicit cleanup did not permit retirement", preview)
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER refuse_retirement_event BEFORE INSERT ON events WHEN NEW.event_type='workers.retired' BEGIN SELECT RAISE(ABORT,'synthetic retirement event failure'); END`); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "POST", path+"/retire", writer, "retire-target", actor, `{}`), 500)
	if count(t, f.store, "worker_retirements") != 0 {
		t.Fatal("failed retirement event left a retired identity")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER refuse_retirement_event`); err != nil {
		t.Fatal(err)
	}
	retired := request(f.handler, "POST", path+"/retire", writer, "retire-target", actor, `{}`)
	result := require(t, retired, 200)
	if result["id"] != target || result["revision"] != float64(2) || result["outcome"] != "retired" {
		t.Fatal(result)
	}
	before = count(t, f.store, "events")
	replay := request(f.handler, "POST", path+"/retire", writer, "retire-target", actor, `{}`)
	if replay.Code != 200 || !bytes.Equal(retired.Body.Bytes(), replay.Body.Bytes()) || count(t, f.store, "events") != before {
		t.Fatal("retirement replay changed original receipt or history", replay.Body.String())
	}
	unchanged := require(t, request(f.handler, "POST", path+"/retire", writer, "already-retired", actor, `{}`), 200)
	if unchanged["outcome"] != "unchanged" || count(t, f.store, "events") != before {
		t.Fatal("repeat retirement created another transition", unchanged)
	}
	preview = retirementStatus(t, f, path+"/retirement", writer)
	if !preview.Retired || preview.CanRetire {
		t.Fatal(preview)
	}
	active := request(f.handler, "GET", base+"/workers", writer, "", "", "")
	require(t, active, 200)
	if strings.Contains(active.Body.String(), target) || strings.Contains(active.Body.String(), `"retired":false`) {
		t.Fatal("retired worker remained an ordinary target", active.Body.String())
	}
	archive := request(f.handler, "GET", base+"/workers?include_retired=true", writer, "", "", "")
	require(t, archive, 200)
	if !strings.Contains(archive.Body.String(), target) || !strings.Contains(archive.Body.String(), `"retired":true`) {
		t.Fatal("archived worker disappeared", archive.Body.String())
	}
	if direct := require(t, request(f.handler, "GET", path, writer, "", target, ""), 200); direct["retired"] != true {
		t.Fatal("historical worker attribution/authentication was revoked", direct)
	}
	// Historical corrections stay available, while reopening/attaching new
	// unfinished work and sending to or from the retired identity are refused.
	retirementJSON(t, f, "PATCH", base+"/jobs/"+completed, writer, actor, map[string]any{"expected_revision": 1, "brief": "Historical correction"}, 200)
	for _, call := range []struct{ method, route, acting, body string }{
		{"POST", base + "/jobs", actor, `{"title":"New","worker_id":"` + target + `"}`},
		{"PATCH", base + "/jobs/" + completed, actor, `{"expected_revision":2,"status":"active"}`},
		{"PATCH", base + "/notes/" + direct, actor, `{"expected_revision":2,"status":"pending"}`},
		{"POST", base + "/notes", actor, `{"worker_id":"` + target + `","job_id":"","phase":"","kind":"reminder","body":"New"}`},
		{"POST", base + "/notes", actor, `{"worker_id":"","job_id":"` + completed + `","phase":"","kind":"reminder","body":"New"}`},
		{"POST", base + "/messages", actor, `{"recipient_worker_id":"` + target + `","body":"New"}`},
		{"POST", base + "/messages", target, `{"recipient_worker_id":"` + actor + `","body":"New"}`},
	} {
		value := require(t, request(f.handler, call.method, call.route, writer, core.NewID("refused"), call.acting, call.body), 409)
		if value["error"].(map[string]any)["code"] != "worker_retired" {
			t.Fatal("new retired target was not refused", value)
		}
	}
	retirementJSON(t, f, "PATCH", base+"/notes/"+direct, writer, actor, map[string]any{"expected_revision": 2, "body": "Historical note correction"}, 200)
	retirementJSON(t, f, "GET", base+"/jobs/"+completed+"/history", writer, target, nil, 200)
	var columns int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('workers')`).Scan(&columns); err != nil || columns != 3 {
		t.Fatal("retirement rewrote base worker history schema", columns, err)
	}
}

func TestWorkerRetirementAdmissionRaceAndBoundedBlockers(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	base := "/api/v1/projects/" + project
	path := base + "/workers/" + target
	start := make(chan struct{})
	retire, assign := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		retire <- request(f.handler, "POST", path+"/retire", f.admin, "race-retire", actor, `{}`)
	}()
	go func() {
		<-start
		assign <- request(f.handler, "POST", base+"/jobs", f.admin, "race-job", actor, `{"title":"Racing assignment","worker_id":"`+target+`"}`)
	}()
	close(start)
	r, a := <-retire, <-assign
	if !(r.Code == 200 && a.Code == 409 || r.Code == 409 && a.Code == 201) {
		t.Fatal("retirement and new assignment both succeeded or lost state", r.Code, r.Body.String(), a.Code, a.Body.String())
	}
	bounded := f.worker(t, project, "Many notes")
	for i := range 21 {
		retirementJSON(t, f, "POST", base+"/notes", f.admin, actor, map[string]any{"worker_id": bounded, "job_id": "", "phase": "", "kind": "reminder", "body": fmt.Sprintf("Pending %d", i)}, 201)
	}
	preview := retirementStatus(t, f, base+"/workers/"+bounded+"/retirement", f.admin)
	if preview.Blockers.Notes.Count != 21 || len(preview.Blockers.Notes.IDs) != 20 || preview.CanRetire {
		t.Fatal("blocker response truncated its actual count or exceeded ID bound", preview)
	}
}

func TestWorkerRetirementManagementAckReplayReauthorizesAndIsolates(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	base := "/api/v1/projects/" + project
	writer, manager := "ack-writer", "ack-manager"
	f.grant(t, project, writer, true)
	id := projectManagerGrant(t, f, project, manager)
	message := retirementJSON(t, f, "POST", base+"/messages", writer, actor, map[string]any{"recipient_worker_id": target, "body": "Incoming"}, 201)["id"].(string)
	path := base + "/messages/" + message + "/ack"
	body := `{"worker_id":"` + target + `"}`
	for _, invalid := range []string{`{"worker_id":null}`, `{"worker_id":""}`} {
		require(t, request(f.handler, "POST", path, writer, core.NewID("invalid-ack"), actor, invalid), 400)
	}
	require(t, request(f.handler, "POST", path, writer, "ordinary-other-ack", actor, body), 403)
	original := request(f.handler, "POST", path, manager, "managed-ack", actor, body)
	require(t, original, 200)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id=? AND project_id=?`, id, project); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "POST", path, manager, "managed-ack", actor, body), 403)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE credential_id=? AND project_id=?`, id, project); err != nil {
		t.Fatal(err)
	}
	replay := request(f.handler, "POST", path, manager, "managed-ack", actor, body)
	if replay.Code != 200 || !bytes.Equal(original.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("recovery changed the original explicit acknowledgement", replay.Body.String())
	}
	// Default and explicit-own acknowledgement preserve ordinary writer access.
	for i, ackBody := range []string{`{}`, body} {
		message := retirementJSON(t, f, "POST", base+"/messages", writer, actor, map[string]any{"recipient_worker_id": target, "body": "Own incoming"}, 201)["id"].(string)
		require(t, request(f.handler, "POST", base+"/messages/"+message+"/ack", writer, fmt.Sprintf("own-%d", i), target, ackBody), 200)
	}
	other := f.project(t, "owner/two")
	foreign := f.worker(t, other, "Foreign")
	require(t, request(f.handler, "GET", base+"/workers/"+foreign+"/retirement", writer, "", "", ""), 404)
	require(t, request(f.handler, "POST", base+"/workers/"+foreign+"/retire", writer, "foreign-retire", actor, `{}`), 404)
	require(t, request(f.handler, "POST", "/api/v1/projects/"+other+"/workers/"+foreign+"/retire", writer, "other-project-retire", "", `{}`), 403)
	reader := "retirement-reader"
	f.grant(t, project, reader, false)
	require(t, request(f.handler, "GET", base+"/workers/"+target+"/retirement", reader, "", "", ""), 200)
	require(t, request(f.handler, "POST", base+"/workers/"+target+"/retire", reader, "reader-retire", "", `{}`), 403)
}

func TestWorkerListRawQueryBound(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/query-bound")
	base := "/api/v1/projects/" + project + "/workers?"
	// Leading zeroes retain a valid page limit, distinguishing the raw byte
	// bound from cursor/value validation.
	for _, size := range []int{16 << 10, (16 << 10) + 1} {
		query := "limit=" + strings.Repeat("0", size-len("limit=1")) + "1"
		status := 200
		if size > 16<<10 {
			status = 400
		}
		require(t, request(f.handler, "GET", base+query, f.admin, "", "", ""), status)
	}
}
