package server_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func requireWorkerNameError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	if value := require(t, response, http.StatusConflict); value["error"].(map[string]any)["code"] != code {
		t.Fatalf("expected %s, got %v", code, value)
	}
}

func TestWorkerNameReusePreservesRetiredHistoriesAndReceipts(t *testing.T) {
	f, project, actor, original := retirementFixture(t)
	base := "/api/v1/projects/" + project
	workers := base + "/workers"
	writer, manager := "name-writer", "name-manager"
	f.grant(t, project, writer, true)
	projectManagerGrant(t, f, project, manager)
	rename := func(id, key, body string) *httptest.ResponseRecorder {
		return request(f.handler, "POST", workers+"/"+id+"/rename", manager, key, actor, body)
	}
	retire := func(id string) {
		t.Helper()
		result := retirementJSON(t, f, "POST", workers+"/"+id+"/retire", writer, actor, map[string]any{}, http.StatusOK)
		if result["id"] != id || result["outcome"] != "retired" {
			t.Fatal("worker was not retired", result)
		}
	}
	completedJob := func(id string) string {
		t.Helper()
		return retirementJSON(t, f, "POST", base+"/jobs", writer, actor, map[string]any{
			"title": "Completed work", "worker_id": id, "status": "completed",
		}, http.StatusCreated)["id"].(string)
	}

	const originalRename = `{"expected_name":"Retiring","name":"Shared"}`
	firstRename := rename(original, "original-rename", originalRename)
	require(t, firstRename, http.StatusOK)
	firstJob := completedJob(original)
	retire(original)
	requireWorkerNameError(t, rename(original, "rename-retired", `{"expected_name":"Shared","name":"Different"}`), "worker_retired")

	const createBody = `{"name":"Shared"}`
	created := request(f.handler, "POST", workers, writer, "reuse-create", actor, createBody)
	recreated := require(t, created, http.StatusOK)["id"].(string)
	if recreated == original {
		t.Fatal("creation reused the archived worker ID")
	}
	secondJob := completedJob(recreated)
	candidate := f.worker(t, project, "Candidate")
	const candidateRename = `{"expected_name":"Candidate","name":"Shared"}`
	requireWorkerNameError(t, request(f.handler, "POST", workers, writer, "duplicate-create", actor, createBody), "worker_already_configured")
	requireWorkerNameError(t, rename(candidate, "duplicate-rename", candidateRename), "worker_already_configured")
	retire(recreated)

	thirdJob := completedJob(candidate)
	if value := require(t, rename(candidate, "reuse-rename", candidateRename), http.StatusOK); value["id"] != candidate || value["name"] != "Shared" {
		t.Fatal("rename did not preserve identity while taking the retired name", value)
	}
	for _, history := range []struct {
		worker, job string
		retired     bool
	}{{original, firstJob, true}, {recreated, secondJob, true}, {candidate, thirdJob, false}} {
		worker := require(t, request(f.handler, "GET", workers+"/"+history.worker, writer, "", actor, ""), http.StatusOK)
		if worker["id"] != history.worker || worker["name"] != "Shared" || (worker["retired"] == true) != history.retired {
			t.Fatal("worker name reuse changed the stored identity or retirement", worker)
		}
		job := require(t, request(f.handler, "GET", base+"/jobs/"+history.job, writer, "", actor, ""), http.StatusOK)
		if job["worker_id"] != history.worker {
			t.Fatal("worker name reuse moved a historical assignment", job)
		}
	}
	replayedCreate := request(f.handler, "POST", workers, writer, "reuse-create", actor, createBody)
	replayedRename := rename(original, "original-rename", originalRename)
	if replayedCreate.Code != http.StatusOK || !bytes.Equal(created.Body.Bytes(), replayedCreate.Body.Bytes()) ||
		replayedRename.Code != http.StatusOK || !bytes.Equal(firstRename.Body.Bytes(), replayedRename.Body.Bytes()) {
		t.Fatal("name reuse changed an earlier receipt", replayedCreate.Body.String(), replayedRename.Body.String())
	}
}

func TestWorkerNameCreateAndRenameRaceForRetiredName(t *testing.T) {
	f, project, actor, original := retirementFixture(t)
	workers := "/api/v1/projects/" + project + "/workers"
	require(t, request(f.handler, "POST", workers+"/"+original+"/retire", f.admin, "race-retire-name", actor, `{}`), http.StatusOK)
	candidate := f.worker(t, project, "Candidate")
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		<-start
		results <- request(f.handler, "POST", workers, f.admin, "race-create-name", actor, `{"name":"Retiring"}`)
	}()
	go func() {
		<-start
		results <- request(f.handler, "POST", workers+"/"+candidate+"/rename", f.admin, "race-rename-name", actor, `{"expected_name":"Candidate","name":"Retiring"}`)
	}()
	close(start)
	winner, loser := <-results, <-results
	if loser.Code == http.StatusOK {
		winner, loser = loser, winner
	}
	id := require(t, winner, http.StatusOK)["id"]
	if id == original {
		t.Fatal("winner reused the retired worker ID")
	}
	requireWorkerNameError(t, loser, "worker_already_configured")
	var active int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM workers w WHERE w.project_id=? AND w.name='Retiring'
		AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)`, project).Scan(&active); err != nil || active != 1 {
		t.Fatal("race did not leave exactly one active worker with the reclaimed name", active, err)
	}
}
