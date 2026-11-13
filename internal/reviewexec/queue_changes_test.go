package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func queueChangeHTTP(t *testing.T, f *attemptFixture, token, key, action string, r QueueChange) Receipt {
	t.Helper()
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/"+action, token, key, encoded(r))
	var out Receipt
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return out
}
func TestQueueChangeOrdinaryWriterSingleIntentPreservesConfigurationAndControls(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	ctx := t.Context()
	read := func() ExecutionConfiguration {
		v, err := f.e.ReadExecutionConfiguration(ctx, f.scope(t, attemptProject, f.writer, false))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	original := read()
	control := roundRead(t, f, 2, "cli")
	// No provider catalog is necessary for local queue planning.
	var err error
	f.e, err = New(f.store, nil, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	before := int64(4)
	request := QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 3, Before: &before}
	first := queueChangeHTTP(t, f, f.writer, "worker-add", "add", request)
	if first.Revision != 2 || !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{2, 3, 4}) {
		t.Fatal(first)
	}
	actual := read()
	actual.Queue = original.Queue
	actual.Revision = original.Revision
	if !reflect.DeepEqual(actual, original) || !reflect.DeepEqual(control, roundRead(t, f, 2, "cli")) {
		t.Fatal("queue edit changed unrelated state", actual, original)
	}
	assertEnrollmentDefault(t, f, 3, "cli")
	assertEnrollmentDefault(t, f, 3, "hosted")
	move := QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 4, Position: "first"}
	queueChangeHTTP(t, f, f.writer, "worker-move", "move", move)
	if got := queueNumbers(configuredQueue(t, f)); !reflect.DeepEqual(got, []int64{4, 2, 3}) {
		t.Fatal(got)
	}
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	if replay := queueChangeHTTP(t, f, f.writer, "worker-add", "add", request); replay != first || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("replay changed queue", replay)
	}
	projectionBefore := projectionDigest(t, f, "")
	configurationBefore := read()
	eventsBefore := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	noop := queueChangeHTTP(t, f, f.writer, "noop", "move", QueueChange{ExpectedConfigurationRevision: 3, PullNumber: 4, Position: "first"})
	if noop.State != "unchanged" || noop.Revision != 3 || projectionBefore != projectionDigest(t, f, "") || !reflect.DeepEqual(configurationBefore, read()) || eventsBefore != attemptCount(t, f.store, `SELECT count(*) FROM events`) {
		t.Fatal("no-op churned", noop)
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("queue edit did provider work", table)
		}
	}
}
func TestQueueChangeAdmissionStrictnessAndAtomicRollback(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/queue/"
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	for _, test := range []struct {
		token, action, body string
		code                int
	}{
		{f.reader, "add", `{"expected_configuration_revision":1,"pull_number":3}`, 403},
		{f.other, "add", `{"expected_configuration_revision":1,"pull_number":3}`, 403},
		{f.writer, "add", `{"expected_configuration_revision":0,"pull_number":3}`, 400},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":3,"paused":false}`, 400},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":3,"before":null}`, 400},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":3,"before":4,"after":2}`, 400},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":3,"position":null}`, 400},
		{f.writer, "move", `{"expected_configuration_revision":1,"pull_number":2}`, 400},
		{f.writer, "move", `{"expected_configuration_revision":1,"pull_number":2,"before":2}`, 400},
		{f.writer, "add", `{"expected_configuration_revision":2,"pull_number":3}`, 409},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":2}`, 409},
		{f.writer, "move", `{"expected_configuration_revision":1,"pull_number":3,"position":"first"}`, 409},
		{f.writer, "add", `{"expected_configuration_revision":1,"pull_number":3,"before":99}`, 409},
	} {
		w := f.request("POST", route+test.action, test.token, "reject-"+test.action, test.body)
		if w.Code != test.code {
			t.Fatal(test, w.Code, w.Body.String())
		}
		if state != projectionDigest(t, f, "")+enrollmentState(t, f) {
			t.Fatal("rejection mutated state")
		}
	}
	admissionSQL(t, f, `CREATE TRIGGER refuse_queue_change BEFORE INSERT ON events WHEN NEW.event_type='reviews.configured' BEGIN SELECT RAISE(ABORT,'queue rollback'); END`)
	request := QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 3}
	_, err := f.e.ChangeQueue(context.Background(), f.scope(t, attemptProject, f.writer, false), "rollback", "add", request)
	if err == nil || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("partial queue/config/default update", err)
	}
	admissionSQL(t, f, `DROP TRIGGER refuse_queue_change`)
	first := queueChangeHTTP(t, f, f.writer, "rollback", "add", request)
	if first.Revision != 2 {
		t.Fatal(first)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	// The original wire action is distinct from an opposite mutation under its key.
	w := f.request("POST", route+"add", f.writer, "rollback", encoded(QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 5}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("idempotency")) {
		t.Fatal(w.Code, w.Body.String())
	}
}
