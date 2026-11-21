package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func retireQueueOnly(t *testing.T, f *attemptFixture, number int64) {
	t.Helper()
	q := configuredQueue(t, f)
	r := reviews.QueueEditRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "management-retirement", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: number, Reason: "superseded"}}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, fmt.Sprintf("retire-only-%d-%d", number, q.Revision), encoded(r))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func assertStoredQueueOrder(t *testing.T, f *attemptFixture, revision int64, execution, visible []int64) {
	t.Helper()
	c, err := f.e.ReadExecutionConfiguration(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader))
	if err != nil || c.Revision != revision || !reflect.DeepEqual(c.Queue, execution) || !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), visible) {
		t.Fatal("execution/visible order mismatch", c, err, queueNumbers(configuredQueue(t, f)))
	}
}

func TestQueueRestorePreservesEvidenceMetadataControlsAndExactReplay(t *testing.T) {
	f := queueEnrollmentFixture(t)
	p := projectionPull(2)
	draft := true
	p.Diff, p.IsDraft = &reviews.PullDiff{Files: 3, Additions: 8, Deletions: 2}, &draft
	p.MergeStateStatus = "CLEAN"
	p.Runs = []reviews.RunInput{{SourceRunID: "retained-review", Channel: "cli", HeadSHA: p.HeadSHA, Reviewer: "Independent", Scope: "patch", Outcome: "completed", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:02:00Z", Attributable: true, Complete: true, Finalized: true, Found: 1, Findings: []reviews.Finding{{SourceFindingID: "retained-finding", Title: "Retained source finding", Detail: "Original decision evidence", Severity: "low", Disposition: "rejected"}}}}
	observation := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "retained-evidence", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{p, projectionPull(4)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "restore-evidence", encoded(observation))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	configureQueue(t, f, 0, 2, 4)
	control := roundRead(t, f, 2, "cli")
	roundGrant(t, f, 2, "cli", control.Revision, 2, 9, "custom-before-retirement")
	a, evidence := roundCapture(t, f, 701, false)
	roundFinalize(t, f, a, evidence, 701, false)
	control = roundRead(t, f, 2, "cli")
	if control.Used != 1 || control.Dry != 1 {
		t.Fatal("fixture did not earn retained round credit", control)
	}
	completions := snapshotTable(t, f, "review_round_completions")
	var before reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &before)
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &runs)
	var detail reviews.RunDetailResponse
	pinned := projectionRead(t, f, "/runs/"+runs.Items[0].ID+"?snapshot="+before.SnapshotID, &detail)
	queueChangeHTTP(t, f, f.writer, "retire-before-restore", "remove", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "superseded"})
	// Ordinary enrollment may never serve as an implicit restore.
	w = f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/add", f.writer, "not-a-restore", encoded(QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 2}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("review_target_retired")) {
		t.Fatal(w.Code, w.Body.String())
	}
	r := QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 2}
	first := queueChangeHTTP(t, f, f.writer, "explicit-restore", "restore", r)
	if first.Revision != 3 || first.State != "configured" {
		t.Fatal(first)
	}
	assertStoredQueueOrder(t, f, 3, []int64{4, 2}, []int64{4, 2})
	var after reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &after)
	if !reflect.DeepEqual(before.Pull, after.Pull) || !reflect.DeepEqual(control, roundRead(t, f, 2, "cli")) || completions != snapshotTable(t, f, "review_round_completions") || !bytes.Equal(pinned, projectionRead(t, f, "/runs/"+runs.Items[0].ID+"?snapshot="+before.SnapshotID, &detail)) {
		t.Fatal("restore changed retained metadata, review evidence or controls", before.Pull, after.Pull)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_current_retired WHERE project_id=? AND number=2`, attemptProject) != 0 {
		t.Fatal("restore retained current retirement marker")
	}
	queueChangeHTTP(t, f, f.writer, "later-order", "move", QueueChange{ExpectedConfigurationRevision: 3, PullNumber: 2, Position: "first"})
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	if replay := queueChangeHTTP(t, f, f.writer, "explicit-restore", "restore", r); replay != first || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("restore replay changed later state", replay)
	}
	w = f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/restore", f.writer, "new-unretired-restore", encoded(QueueChange{ExpectedConfigurationRevision: 4, PullNumber: 2}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("review_target_not_retired")) || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("new restore on unretired PR did not refuse", w.Code, w.Body.String())
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueRestoreAlreadyEnrolledPreservesPositionAndPassiveRefreshCannotRestore(t *testing.T) {
	for _, destination := range []string{"", "last"} {
		t.Run("destination-"+destination, func(t *testing.T) {
			f := queueEnrollmentFixture(t)
			configureQueue(t, f, 0, 2, 4, 6)
			var pins map[int64]reviews.SourceMetadataPin
			roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
				var err error
				pins, err = reviews.PinSourceMetadata(ctx, tx, attemptProject, []int64{2})
				return err
			})
			retireQueueOnly(t, f, 2)
			state := projectionDigest(t, f, "")
			scope := f.scope(t, attemptProject, f.writer, false)
			roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
				p := projectionPull(2)
				p.Complete, p.CIState, p.Mergeability = false, "", ""
				_, err := reviews.PublishSourceMetadata(ctx, tx, scope, 801, pins, []reviews.PullInput{p})
				return err
			})
			if state != projectionDigest(t, f, "") {
				t.Fatal("passive source refresh restored a retired PR")
			}
			visibleRevision := configuredQueue(t, f).Revision
			queueChangeHTTP(t, f, f.writer, "restore-enrolled", "restore", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Position: destination})
			want := []int64{2, 4, 6}
			if destination == "last" {
				want = []int64{4, 6, 2}
			}
			assertStoredQueueOrder(t, f, 2, want, want)
			if configuredQueue(t, f).Revision != visibleRevision+1 {
				t.Fatal("visibility restore failed to advance the workspace revision")
			}
		})
	}
}

func TestQueueRestoreHistoricalRetirementWithMissingParentUsesHonestPlaceholder(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	retireQueueOnly(t, f, 2)
	// Complete replacement observations can remove both current parent and
	// marker, but the historical retirement still requires explicit restore.
	for i, retired := range [][]reviews.RetiredPull{{{Number: 2, Reason: "superseded"}}, {}} {
		q := configuredQueue(t, f)
		obs := reviews.ObservationRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "replacement", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{projectionPull(4)}, Retired: retired}
		w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, fmt.Sprintf("replace-%d", i), encoded(obs))
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls WHERE project_id=? AND number=2`, attemptProject) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_current_retired WHERE project_id=? AND number=2`, attemptProject) != 0 {
		t.Fatal("fixture did not remove current parent and marker")
	}
	configureQueue(t, f, 1, 4)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/add", f.writer, "historical-add", encoded(QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 2}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("review_target_retired")) {
		t.Fatal(w.Code, w.Body.String())
	}
	anchor := int64(4)
	queueChangeHTTP(t, f, f.writer, "historical-restore", "restore", QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 2, Before: &anchor})
	assertStoredQueueOrder(t, f, 3, []int64{2, 4}, []int64{2, 4})
	q := configuredQueue(t, f)
	p := q.Items[0]
	if p.Title != "PR #2" || p.State != "unknown" || p.HeadSHA != "" || p.BaseSHA != "" || p.CIState != "unknown" || p.Mergeability != "unknown" || p.Complete {
		t.Fatal("missing-parent restore invented metadata", p)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueReorderSelectedSlotsNoopRevisionAndReplay(t *testing.T) {
	f := queueEnrollmentFixture(t)
	// Visible entries not enrolled in execution retain their slots as well.
	obs := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "retained", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{projectionPull(1), projectionPull(2), projectionPull(3), projectionPull(4), projectionPull(5), projectionPull(6)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "reorder-retained", encoded(obs))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	configureQueue(t, f, 0, 2, 4, 6)
	controls := snapshotTable(t, f, "review_round_controls")
	r := QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{6, 2}}
	first := queueChangeHTTP(t, f, f.writer, "selected-order", "reorder", r)
	assertStoredQueueOrder(t, f, 2, []int64{6, 4, 2}, []int64{1, 6, 3, 4, 5, 2})
	projectionBefore := projectionDigest(t, f, "")
	configurationBefore := snapshotTable(t, f, "review_execution_config")
	eventsBefore := snapshotTable(t, f, "events")
	noop := queueChangeHTTP(t, f, f.writer, "same-order", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{6, 2}})
	// A no-op creates its receipt, but no configuration, projection or event.
	if noop.State != "unchanged" || noop.Revision != 2 || controls != snapshotTable(t, f, "review_round_controls") || projectionBefore != projectionDigest(t, f, "") || configurationBefore != snapshotTable(t, f, "review_execution_config") || eventsBefore != snapshotTable(t, f, "events") {
		t.Fatal(noop)
	}
	queueChangeHTTP(t, f, f.writer, "later-reorder", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{2, 6}})
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	if replay := queueChangeHTTP(t, f, f.writer, "selected-order", "reorder", r); replay != first || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("reorder replay changed current order", replay)
	}
	if replay := queueChangeHTTP(t, f, f.writer, "same-order", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{6, 2}}); replay != noop || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("no-op replay changed current order or revision", replay)
	}
	w = f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/reorder", f.writer, "selected-order", encoded(QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{2, 6}}))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("idempotency")) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestQueueReorderBoundsAndExistingRequestBytes(t *testing.T) {
	queue := make([]int64, 100)
	order := make([]int64, 100)
	for i := range queue {
		queue[i], order[i] = int64(i+1), int64(100-i)
	}
	r := QueueChange{ExpectedConfigurationRevision: 1, Order: order}
	if err := validateQueueChange("reorder", r); err != nil {
		t.Fatal("maximum bounded order refused", err)
	}
	got, err := changedQueue(queue, "reorder", r)
	if err != nil || !reflect.DeepEqual(got, order) || queue[0] != 1 {
		t.Fatal("maximum reorder changed input or omitted selected slots", got, err)
	}
	r.Order = append(r.Order, 101)
	if err := validateQueueChange("reorder", r); err == nil {
		t.Fatal("unbounded reorder accepted")
	}
	for _, tc := range []struct {
		request QueueChange
		body    string
	}{
		{QueueChange{ExpectedConfigurationRevision: 7, PullNumber: 42}, `{"expected_configuration_revision":7,"pull_number":42}`},
		{QueueChange{ExpectedConfigurationRevision: 7, PullNumber: 42, Position: "first"}, `{"expected_configuration_revision":7,"pull_number":42,"position":"first"}`},
		{QueueChange{ExpectedConfigurationRevision: 7, PullNumber: 42, Reason: "superseded"}, `{"expected_configuration_revision":7,"pull_number":42,"reason":"superseded"}`},
		{QueueChange{ExpectedConfigurationRevision: 7, Order: []int64{42, 17}}, `{"expected_configuration_revision":7,"order":[42,17]}`},
	} {
		if got := encoded(tc.request); got != tc.body {
			t.Fatal("queue request wire bytes changed", got, tc.body)
		}
	}
}

func TestQueueRestoreReorderStrictScopedAdmission(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	retireQueueOnly(t, f, 2)
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	for i, tc := range []struct {
		token, action, body, code string
		status                    int
	}{
		{f.reader, "restore", `{"expected_configuration_revision":1,"pull_number":2}`, "forbidden", 403},
		{f.other, "restore", `{"expected_configuration_revision":1,"pull_number":2}`, "forbidden", 403},
		{f.reader, "reorder", `{"expected_configuration_revision":1,"order":[4,2]}`, "forbidden", 403},
		{f.other, "reorder", `{"expected_configuration_revision":1,"order":[4,2]}`, "forbidden", 403},
		{f.writer, "restore", `{"expected_configuration_revision":2,"pull_number":2}`, "revision_conflict", 409},
		{f.writer, "reorder", `{"expected_configuration_revision":2,"order":[4,2]}`, "revision_conflict", 409},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":4}`, "review_target_not_retired", 409},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":99}`, "review_target_not_retired", 409},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[4,99]}`, "review_not_configured", 409},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":2,"reason":""}`, "validation_failed", 400},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":2,"order":[]}`, "validation_failed", 400},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":2,"before":null}`, "validation_failed", 400},
		{f.writer, "restore", `{"expected_configuration_revision":1,"pull_number":2,"before":4,"position":"first"}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[4,2],"pull_number":0}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[4,2],"position":""}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":null}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[]}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[2]}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[2,2]}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[0,4]}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[2,2147483648]}`, "validation_failed", 400},
		{f.writer, "reorder", `{"expected_configuration_revision":1,"order":[2,4],"order":[4,2]}`, "validation_failed", 400},
	} {
		w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/"+tc.action, tc.token, fmt.Sprintf("reject-new-queue-%d", i), tc.body)
		if w.Code != tc.status || !bytes.Contains(w.Body.Bytes(), []byte(tc.code)) || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
}

func TestQueueRestoreReorderAtomicFailureAndMissingProjection(t *testing.T) {
	for _, action := range []string{"restore", "reorder"} {
		for _, event := range []string{"review.observed", "reviews.configured"} {
			t.Run(action+"/"+event, func(t *testing.T) {
				f := queueEnrollmentFixture(t)
				configureQueue(t, f, 0, 2, 4)
				r := QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{4, 2}}
				if action == "restore" {
					queueChangeHTTP(t, f, f.writer, "remove-atomic", "remove", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "defunct"})
					r = QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 2}
				}
				state := projectionDigest(t, f, "") + enrollmentState(t, f)
				admissionSQL(t, f, `CREATE TRIGGER refuse_new_queue_event BEFORE INSERT ON events WHEN NEW.event_type='`+event+`' BEGIN SELECT RAISE(ABORT,'atomic queue failure'); END`)
				w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/execution/queue/"+action, f.writer, "atomic-new-queue", encoded(r))
				if w.Code < 400 || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
					t.Fatal("partial queue transaction", w.Code, w.Body.String())
				}
				admissionSQL(t, f, `DROP TRIGGER refuse_new_queue_event`)
				queueChangeHTTP(t, f, f.writer, "atomic-new-queue", action, r)
			})
		}
	}
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2, 4)
	retireQueueOnly(t, f, 2)
	f.e.publishQueue = nil
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	_, err := f.e.ChangeQueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "no-projection", "restore", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2})
	attemptCode(t, err, "review_projection_unavailable")
	if state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("restore without native projection changed configuration")
	}
}
