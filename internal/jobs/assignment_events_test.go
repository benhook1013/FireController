package jobs_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func assignmentEvents(t *testing.T, a *testAPI) []core.Event {
	t.Helper()
	rows, err := a.store.DB().Query(`SELECT event_type,resource_id,revision,payload_json FROM events WHERE event_type IN ('job.assigned','job.unassigned') ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var e core.Event
		var payload string
		if err := rows.Scan(&e.Type, &e.ResourceID, &e.Revision, &payload); err != nil {
			t.Fatal(err)
		}
		e.Payload = json.RawMessage(payload)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestAssignmentEventsOnlyNotifyOtherAffectedWorkers(t *testing.T) {
	a, project, token, first, path := progressFixture(t)
	second, acting := a.createWorker(project, "second"), a.createWorker(project, "dispatcher")
	edit := func(actor, key string, revision int, fields map[string]any) {
		t.Helper()
		fields["expected_revision"] = revision
		noteOK(t, a.callWorker("PATCH", path, token, actor, key, fields), 200)
	}
	edit(first, "self-assign", 1, map[string]any{"worker_id": first})
	if len(assignmentEvents(t, a)) != 0 {
		t.Fatal("self assignment notified the acting worker")
	}
	edit(first, "handoff", 2, map[string]any{"worker_id": second})
	edit(first, "handoff", 2, map[string]any{"worker_id": second}) // exact replay
	edit(second, "routine", 3, map[string]any{"status": "blocked", "progress": map[string]any{"summary": "PRIVATE_PROGRESS"}})
	events := assignmentEvents(t, a)
	if len(events) != 1 || events[0].Type != "job.assigned" || events[0].Revision != 3 {
		t.Fatal("handoff, replay or routine edits produced wrong attention events", events)
	}
	edit(acting, "transfer-both", 4, map[string]any{"worker_id": first})
	// An administrator without a bound acting worker has no sender metadata.
	noteOK(t, a.call("PATCH", path, a.admin, "admin-unassign", map[string]any{"expected_revision": 5, "worker_id": ""}), 200)
	events = assignmentEvents(t, a)
	wantKinds := []string{"job.assigned", "job.unassigned", "job.assigned", "job.unassigned"}
	wantRecipients := []string{second, second, first, first}
	wantSenders := []string{first, acting, acting, ""}
	if len(events) != len(wantKinds) {
		t.Fatal("ownership changes lost affected workers", events)
	}
	for i, event := range events {
		var payload map[string]string
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if event.Type != wantKinds[i] || event.ResourceID != strings.TrimPrefix(path, "/api/v1/projects/"+project+"/jobs/") || payload["recipient_worker_id"] != wantRecipients[i] || payload["sender_worker_id"] != wantSenders[i] || len(payload) != 2 && !(i == 3 && len(payload) == 1) {
			t.Fatal("assignment payload has wrong ownership or private fields", event, payload)
		}
	}
	create := map[string]any{"title": "PRIVATE_TITLE", "brief": "PRIVATE_BRIEF", "worker_id": second}
	noteOK(t, a.callWorker("POST", "/api/v1/projects/"+project+"/jobs", token, first, "create-assigned", create), 201)
	noteOK(t, a.callWorker("POST", "/api/v1/projects/"+project+"/jobs", token, first, "create-assigned", create), 201)
	create["worker_id"] = first
	noteOK(t, a.callWorker("POST", "/api/v1/projects/"+project+"/jobs", token, first, "create-self", create), 201)
	events = assignmentEvents(t, a)
	if len(events) != 5 || events[4].Type != "job.assigned" || events[4].Revision != 1 || strings.Contains(string(events[4].Payload), "PRIVATE") {
		t.Fatal("job creation attention was missing, duplicated or leaked text", events)
	}
}

func TestAssignmentEventFailureRollsBackJobHistoryAndReceipt(t *testing.T) {
	a, project, token, worker, path := progressFixture(t)
	other := a.createWorker(project, "dispatcher")
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_assignment BEFORE INSERT ON events WHEN NEW.event_type='job.assigned' BEGIN SELECT RAISE(ABORT,'synthetic assignment failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := contextState(t, a)
	failed := a.callWorker("PATCH", path, token, other, "atomic-assign", map[string]any{"expected_revision": 1, "worker_id": worker})
	if failed.Code < 500 || readWorking(t, a, path, token).WorkerID != "" {
		t.Fatal("assignment failure committed ownership", failed.Code)
	}
	for table, count := range before {
		if contextState(t, a)[table] != count {
			t.Fatal("assignment failure committed partial state", table)
		}
	}
	failed = a.callWorker("POST", "/api/v1/projects/"+project+"/jobs", token, other, "atomic-create", map[string]any{"title": "Atomic", "worker_id": worker})
	if failed.Code < 500 || len(assignmentEvents(t, a)) != 0 {
		t.Fatal("failed job create committed attention", failed.Code)
	}
	for table, count := range before {
		if contextState(t, a)[table] != count {
			t.Fatal("job-create attention failure committed partial state", table)
		}
	}
}
