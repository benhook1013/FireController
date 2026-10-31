package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestQueueRemovalPreservesHistoryControlsAndReplay(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	before := configuredQueue(t, f)
	var original reviews.PullResponse
	pinned := projectionRead(t, f, "/pulls/2?snapshot="+before.SnapshotID, &original)
	controls := attemptCount(t, f.store, `SELECT count(*) FROM review_round_controls`)
	request := QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "superseded"}
	first := queueChangeHTTP(t, f, f.writer, "remove-original", "remove", request)
	if first.Revision != 2 || !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{4}) {
		t.Fatal(first)
	}
	var retained reviews.PullResponse
	if after := projectionRead(t, f, "/pulls/2?snapshot="+before.SnapshotID, &retained); !bytes.Equal(pinned, after) {
		t.Fatal("original history changed")
	}
	if controls != attemptCount(t, f.store, `SELECT count(*) FROM review_round_controls`) {
		t.Fatal("controls changed")
	}
	if replay := queueChangeHTTP(t, f, f.writer, "remove-original", "remove", request); replay != first {
		t.Fatal(replay)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	request = QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 4, Reason: "defunct"}
	if final := queueChangeHTTP(t, f, f.writer, "remove-final", "remove", request); final.Revision != 3 {
		t.Fatal(final)
	}
	var config ExecutionConfiguration
	w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/execution/configuration", f.writer, "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &config) != nil || config.Queue == nil || len(config.Queue) != 0 || config.CapabilityID != "test-capability" || config.Mode != "independent" || config.Paused {
		t.Fatal(w.Code, w.Body.String())
	}
	choice, err := f.e.chooseRun(t.Context(), Run{}, 901, Configuration{Queue: []int64{}}, nil)
	if err != nil || choice.state != "empty" || choice.reason != "queue_complete" {
		t.Fatal(choice, err)
	}
	// Enrollment after an empty queue remains ordinary, without reactivating old PRs.
	if added := queueChangeHTTP(t, f, f.writer, "add-after-empty", "add", QueueChange{ExpectedConfigurationRevision: 3, PullNumber: 8}); added.Revision != 4 {
		t.Fatal(added)
	}
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{8}) {
		t.Fatal("retired targets reappeared")
	}
}

func TestQueueRemovalRefusalsAndAtomicRollback(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/queue/remove"
	before := projectionDigest(t, f, "") + enrollmentState(t, f)
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{f.reader, `{"expected_configuration_revision":1,"pull_number":2,"reason":"defunct"}`, 403},
		{f.other, `{"expected_configuration_revision":1,"pull_number":2,"reason":"defunct"}`, 403},
		{f.writer, `{"expected_configuration_revision":2,"pull_number":2,"reason":"defunct"}`, 409},
		{f.writer, `{"expected_configuration_revision":1,"pull_number":2}`, 400},
		{f.writer, `{"expected_configuration_revision":1,"pull_number":2,"reason":null}`, 400},
		{f.writer, `{"expected_configuration_revision":1,"pull_number":2,"reason":"merged"}`, 400},
		{f.writer, `{"expected_configuration_revision":1,"pull_number":2,"reason":"defunct","position":"last"}`, 400},
		{f.writer, `{"expected_configuration_revision":1,"pull_number":9,"reason":"defunct"}`, 409},
	} {
		w := f.request("POST", route, tc.token, "invalid-remove", tc.body)
		if w.Code != tc.status || before != projectionDigest(t, f, "")+enrollmentState(t, f) {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	admissionSQL(t, f, `CREATE TRIGGER refuse_retirement BEFORE INSERT ON review_retired_versions BEGIN SELECT RAISE(ABORT,'retirement rollback'); END`)
	request := QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "defunct"}
	w := f.request("POST", route, f.writer, "rollback-remove", encoded(request))
	if w.Code < 400 || before != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("partial removal", w.Code, w.Body.String())
	}
	admissionSQL(t, f, `DROP TRIGGER refuse_retirement`)
	queueChangeHTTP(t, f, f.writer, "rollback-remove", "remove", request)
}

func TestQueueRemovalPendingOperationAndMergedRefusals(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	op := f.enqueue(t, attemptProject, "original-review", "cli", 2, 1)
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/queue/remove"
	w := f.request("POST", route, f.writer, "remove-pending", encoded(QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "defunct"}))
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte(`"operation_id":"`+op.ID+`"`)) {
		t.Fatal(w.Code, w.Body.String())
	}
	q := configuredQueue(t, f)
	merged := reviews.QueueEditRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "merged-proof", Retired: []reviews.RetiredPull{}, Queue: []reviews.QueuePullInput{{Number: 4, Title: "Merged source", State: "merged", CIState: "unknown", Mergeability: "unknown"}}}
	edit := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, "record-merged", encoded(merged))
	if edit.Code != 201 {
		t.Fatal(edit.Code, edit.Body.String())
	}
	w = f.request("POST", route, f.writer, "remove-merged", encoded(QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 4, Reason: "defunct"}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("review_queue_merged")) {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{2, 4}) {
		t.Fatal("refused removal changed queue")
	}
}

func TestEmptySchedulingQueueRequiresExplicitArrayAndKeepsFrontFacts(t *testing.T) {
	f := queueEnrollmentFixture(t)
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/configuration"
	for _, body := range []string{
		`{"expected_revision":0,"capability_id":"test-capability","mode":"independent","channels":["cli"],"paused":false}`,
		`{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":null,"channels":["cli"],"paused":false}`,
	} {
		w := f.request("POST", route, f.admin, "missing-array", body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	zero := int64(0)
	config := Configuration{ExpectedRevision: &zero, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{}, Channels: []string{"cli"}}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "explicit-empty", config); err != nil {
		t.Fatal(err)
	}
	frame := frontFrame{repository: 801, configuration: 1, checkedAt: stamp(), prefix: []frontPull{}, complete: true}
	if !validFrontFrame(frame, config, "cli") {
		t.Fatal("empty configured queue produced unavailable front")
	}
}
