package jobs_test

import (
	"fmt"
	"reflect"
	"testing"
)

func TestWorkerRetirementFreezesFocusAndPauseButPreservesReadsAndReplay(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("Retirement focus", "owner/retirement-focus")
	actor := a.createWorker(p, "actor")
	target := a.createWorker(p, "target")
	token := a.grant(p, "synthetic-retirement-focus-writer", "read", "write")
	base := "/api/v1/projects/" + p
	var completed [2]string
	for i := range completed {
		w := noteCall(a, "POST", base+"/jobs", token, actor, fmt.Sprint("completed-job-", i), map[string]any{"title": "Historical work", "status": "completed", "worker_id": target})
		noteOK(t, w, 201)
		var created receipt
		decodeResponse(t, w, &created)
		completed[i] = created.ID
	}
	focusPath := base + "/workers/" + target + "/primary-job"
	pausePath := base + "/workers/" + target + "/pause"
	focusRequest := map[string]any{"expected_revision": 0, "job_id": completed[0]}
	pauseRequest := map[string]any{"expected_revision": 0, "paused": true, "reason": "Retaining historical pause"}
	focusReceipt := noteCall(a, "PUT", focusPath, token, actor, "old-focus", focusRequest)
	pauseReceipt := noteCall(a, "PUT", pausePath, token, actor, "old-pause", pauseRequest)
	noteOK(t, focusReceipt, 200)
	noteOK(t, pauseReceipt, 200)
	if _, err := a.store.DB().Exec(`INSERT INTO worker_retirements VALUES(?,?, '2026-11-01T00:00:00Z','synthetic-actor')`, p, target); err != nil {
		t.Fatal(err)
	}
	before := pauseCounts(t, a)
	for i, req := range []map[string]any{{"expected_revision": 1, "job_id": completed[1]}, {"expected_revision": 1, "job_id": ""}} {
		noteError(t, noteCall(a, "PUT", focusPath, token, actor, fmt.Sprint("new-focus-", i), req), 409, "worker_retired")
	}
	noteError(t, noteCall(a, "PUT", pausePath, token, actor, "new-pause", map[string]any{"expected_revision": 1, "paused": false, "reason": ""}), 409, "worker_retired")
	if !reflect.DeepEqual(before, pauseCounts(t, a)) {
		t.Fatal("retired target mutation changed durable coordination state")
	}
	for _, path := range []string{focusPath, focusPath + "/history", pausePath, pausePath + "/history"} {
		noteOK(t, noteCall(a, "GET", path, token, actor, "", nil), 200)
	}
	for _, replay := range []struct {
		path, key, expected string
		request             map[string]any
	}{{focusPath, "old-focus", focusReceipt.Body.String(), focusRequest}, {pausePath, "old-pause", pauseReceipt.Body.String(), pauseRequest}} {
		got := noteCall(a, "PUT", replay.path, token, actor, replay.key, replay.request)
		noteOK(t, got, 200)
		if got.Body.String() != replay.expected {
			t.Fatal("retirement changed an exact prior receipt")
		}
	}
}

func TestWorkerRetirementPendingNotesAndReopenedJobsRefuseHistoricalTarget(t *testing.T) {
	a := setupAPI(t)
	p := a.createProject("Retirement", "owner/retirement")
	actor := a.createWorker(p, "actor")
	target := a.createWorker(p, "target")
	token := a.grant(p, "synthetic-retirement-writer", "read", "write")
	base := "/api/v1/projects/" + p
	w := noteCall(a, "POST", base+"/jobs", token, actor, "completed-job", map[string]any{"title": "Historical work", "status": "completed", "worker_id": target})
	noteOK(t, w, 201)
	var created receipt
	decodeResponse(t, w, &created)
	if _, err := a.store.DB().Exec(`INSERT INTO worker_retirements VALUES(?,?, '2026-11-01T00:00:00Z','synthetic-actor')`, p, target); err != nil {
		t.Fatal(err)
	}
	for i, req := range []map[string]any{{"expected_revision": 1, "status": "active"}, {"expected_revision": 1, "worker_id": target, "status": "parked"}} {
		noteError(t, noteCall(a, "PATCH", base+"/jobs/"+created.ID, token, actor, fmt.Sprint("reopen-", i), req), 409, "worker_retired")
	}
	for i, req := range []map[string]any{{"worker_id": target, "job_id": "", "phase": "", "kind": "reminder", "body": "Pending direct note"}, {"job_id": created.ID, "worker_id": "", "phase": "", "kind": "reminder", "body": "Pending historical job note"}} {
		noteError(t, noteCall(a, "POST", base+"/notes", token, actor, fmt.Sprint("note-", i), req), 409, "worker_retired")
	}
	// Editing completed history must remain possible without making new work.
	noteOK(t, noteCall(a, "PATCH", base+"/jobs/"+created.ID, token, actor, "history-title", map[string]any{"expected_revision": 1, "title": "Updated historical title"}), 200)
	noteOK(t, noteCall(a, "GET", base+"/jobs/"+created.ID, token, actor, "", nil), 200)
}
