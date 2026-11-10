package jobs_test

import (
	"fmt"
	"testing"
)

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
