package coordimport_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/coordimport"
)

func retireImportedWorker(t *testing.T, f *importFixture, worker string) {
	t.Helper()
	checked[map[string]any](t, f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/workers/"+worker+"/retire", f.admin, f.worker, "retire-"+worker, struct{}{}), 200)
}

func TestWorkerRetirementPeerFreshImportedObligationsRefused(t *testing.T) {
	for _, kind := range []string{"unfinished-job", "unacknowledged-message"} {
		t.Run(kind, func(t *testing.T) {
			f := newImportFixture(t)
			target := f.workerFor(f.project, "Retired")
			batch := preparedBatch(target, 0)
			if kind == "unacknowledged-message" {
				batch = messageBatch(f.worker, target, 0)
			}
			preview := f.preview(batch)
			retireImportedWorker(t, f, target)
			before := f.snapshot(false)
			refusal := f.apply(batch, preview, "cannot-recreate-work")
			errorIs(t, refusal, 409, "worker_retired")
			conserved(t, before, f.snapshot(false))
			errorIs(t, f.call(http.MethodPost, f.base()+"/preview", f.admin, f.worker, "", batch), 409, "worker_retired")
			conserved(t, before, f.snapshot(false))
		})
	}
}

func TestWorkerRetirementPeerHistoricalImportAndMappingReplay(t *testing.T) {
	f := newImportFixture(t)
	target := f.workerFor(f.project, "Historical")
	retireImportedWorker(t, f, target)
	batch := preparedBatch(target, 0)
	batch.Jobs[0].Status = "completed"
	batch.Messages = messageBatch(target, target, 0).Messages
	batch.Messages[0].SeenAt = batch.Messages[0].CreatedAt
	batch.Messages[0].AcknowledgedAt = batch.Messages[0].CreatedAt
	outgoing := messageBatch(target, f.worker, 0).Messages[0]
	outgoing.SourceKey = "historical-outgoing"
	batch.Messages = append(batch.Messages, outgoing)
	original, _ := json.Marshal(batch)
	preview := f.preview(batch)
	first := f.apply(batch, preview, "historical-import")
	receipt := checked[coordimport.Receipt](t, first, 200)
	before := f.snapshot(false)
	replay := f.apply(batch, preview, "historical-import")
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("original receipt replay changed", replay.Code)
	}
	conserved(t, before, f.snapshot(false))
	next := batch
	revision := receipt.Revision
	next.ExpectedRevision = &revision
	unchanged := f.preview(next)
	if unchanged.Counts.NewJobs != 0 || unchanged.Counts.UnchangedJobs != 1 || unchanged.Counts.NewMessages != 0 || unchanged.Counts.UnchangedMessages != 2 {
		t.Fatal("retained mappings were rejected or changed", unchanged.Counts)
	}
	checked[coordimport.Receipt](t, f.apply(next, unchanged, "existing-mappings"), 200)
	actual, _ := json.Marshal(batch)
	if !reflect.DeepEqual(original, actual) {
		t.Fatal("historical source bytes changed")
	}
	assertImportSnapshot(t, f)
}

func TestWorkerRetirementPeerExistingObligationMappingsReplayAfterCleanup(t *testing.T) {
	f := newImportFixture(t)
	target := f.workerFor(f.project, "Original worker")
	batch := preparedBatch(target, 0)
	batch.Messages = messageBatch(f.worker, target, 0).Messages
	preview := f.preview(batch)
	first := f.apply(batch, preview, "original-obligations")
	receipt := checked[coordimport.Receipt](t, first, 200)
	base := "/api/v1/projects/" + f.project
	checked[map[string]any](t, f.call(http.MethodPatch, base+"/jobs/"+receipt.Mappings[0].JobID, f.admin, f.worker, "reassign", map[string]any{"expected_revision": 1, "worker_id": f.worker}), 200)
	checked[map[string]any](t, f.call(http.MethodPost, base+"/messages/"+receipt.MessageMappings[0].MessageID+"/ack", f.admin, f.worker, "ack-original", map[string]any{"worker_id": target}), 200)
	retireImportedWorker(t, f, target)
	before := f.snapshot(false)
	replay := f.apply(batch, preview, "original-obligations")
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("original source receipt was not replayable after cleanup", replay.Code)
	}
	conserved(t, before, f.snapshot(false))
	revision := receipt.Revision
	batch.ExpectedRevision = &revision
	retained := f.preview(batch)
	if retained.Counts.NewJobs != 0 || retained.Counts.NewMessages != 0 || retained.Counts.UnchangedJobs != 1 || retained.Counts.UnchangedMessages != 1 {
		t.Fatal("historical obligation mappings became fresh", retained.Counts)
	}
	checked[coordimport.Receipt](t, f.apply(batch, retained, "retained-obligations"), 200)
	assertImportSnapshot(t, f)
}
