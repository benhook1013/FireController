package server_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestWorkerRenamePreservesIdentityAndRequiresProjectManagement(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	manager, writer := "rename-manager", "rename-writer"
	projectManagerGrant(t, f, project, manager)
	f.grant(t, project, writer, true)
	base := "/api/v1/projects/" + project
	path := base + "/workers/" + target
	rename := path + "/rename"
	job := retirementJSON(t, f, "POST", base+"/jobs", writer, actor, map[string]any{"title": "Assigned work", "worker_id": target}, 201)["id"].(string)
	body := `{"expected_name":"Retiring","name":"New name"}`
	require(t, request(f.handler, "POST", rename, writer, "writer-rename", actor, body), http.StatusForbidden)

	sessionID := strings.Repeat("a", 32)
	cookie := browserActionsLogin(t, f, project, manager, sessionID, true)
	bad := browserActionRequest(f, "POST", rename, sessionID, cookie, actor, "rename-cookie", body)
	bad.Header.Set("Origin", "https://foreign.example")
	require(t, serveBrowserAction(f, bad), http.StatusForbidden)
	send := func(payload string, status int) map[string]any {
		t.Helper()
		return require(t, serveBrowserAction(f, browserActionRequest(f, "POST", rename, sessionID, cookie, actor, "rename-cookie", payload)), status)
	}
	receipt := send(body, http.StatusOK)
	replay := send(body, http.StatusOK)
	if receipt["id"] != target || receipt["name"] != "New name" || replay["name"] != receipt["name"] {
		t.Fatal("rename changed the worker identity or failed exact replay", receipt, replay)
	}
	var events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND resource_id=? AND event_type='workers.renamed'`, project, target).Scan(&events); err != nil || events != 1 {
		t.Fatal("rename did not record exactly one change", events, err)
	}
	for _, test := range []struct {
		key, body string
		status    int
	}{
		{"stale", `{"expected_name":"Retiring","name":"Another name"}`, 409},
		{"duplicate", `{"expected_name":"New name","name":"Actor"}`, 409},
		{"padded", `{"expected_name":"New name","name":" padded "}`, 400},
	} {
		require(t, request(f.handler, "POST", rename, manager, test.key, actor, test.body), test.status)
	}
	other := f.project(t, "owner/two")
	foreignWorker := f.worker(t, other, "Foreign")
	require(t, request(f.handler, "POST", base+"/workers/"+foreignWorker+"/rename", manager, "foreign-rename", actor, `{"expected_name":"Foreign","name":"Another name"}`), 404)
	worker := require(t, request(f.handler, "GET", path, manager, "", actor, ""), 200)
	if worker["id"] != target || worker["name"] != "New name" {
		t.Fatal("worker details did not retain the renamed identity", worker)
	}
	assigned := require(t, request(f.handler, "GET", base+"/jobs/"+job, manager, "", actor, ""), 200)
	if assigned["worker_id"] != target {
		t.Fatal("renaming changed the job assignment", assigned)
	}
}
