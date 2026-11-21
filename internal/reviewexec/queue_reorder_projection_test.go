package reviewexec

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func reorderDeliveryOnly(t *testing.T, f *attemptFixture, order ...int64) {
	t.Helper()
	q := configuredQueue(t, f)
	r := reviews.QueueEditRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "management-order", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{}, Order: order}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, fmt.Sprintf("delivery-order-%d", q.Revision), encoded(r))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestQueueReorderDivergedDeliveryPreservesUnselectedSlots(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 1, 2, 3)
	reorderDeliveryOnly(t, f, 2, 1)
	assertStoredQueueOrder(t, f, 1, []int64{1, 2, 3}, []int64{2, 1, 3})
	q := configuredQueue(t, f)
	controls := snapshotTable(t, f, "review_round_controls")
	pullBodies := snapshotTable(t, f, "review_pull_versions")
	observed := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`)
	configured := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.configured'`)
	r := QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{3, 1}}
	first := queueChangeHTTP(t, f, f.writer, "diverged-reorder", "reorder", r)
	assertStoredQueueOrder(t, f, 2, []int64{3, 2, 1}, []int64{2, 3, 1})
	if first.State != "configured" || first.Revision != 2 || configuredQueue(t, f).Revision != q.Revision+1 || controls != snapshotTable(t, f, "review_round_controls") || pullBodies != snapshotTable(t, f, "review_pull_versions") {
		t.Fatal("scoped reorder changed controls, metadata or revision incorrectly", first)
	}
	if observed+1 != attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) || configured+1 != attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.configured'`) {
		t.Fatal("reorder published duplicate observable effects")
	}
	queueChangeHTTP(t, f, f.writer, "later-diverged-reorder", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{1, 3}})
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	if replay := queueChangeHTTP(t, f, f.writer, "diverged-reorder", "reorder", r); replay != first || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("exact replay republished a selected order", replay)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueReorderExecutionNoopReconcilesDivergedDeliveryAtomically(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 3, 2, 1)
	reorderDeliveryOnly(t, f, 2, 1, 3)
	assertStoredQueueOrder(t, f, 1, []int64{3, 2, 1}, []int64{2, 1, 3})
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/queue/reorder"
	r := QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{3, 1}}
	state := projectionDigest(t, f, "") + enrollmentState(t, f)
	for _, event := range []string{"review.observed", "reviews.configured"} {
		admissionSQL(t, f, `CREATE TRIGGER fail_delivery_reconcile BEFORE INSERT ON events WHEN NEW.event_type='`+event+`' BEGIN SELECT RAISE(ABORT,'reconcile failure'); END`)
		w := f.request("POST", route, f.writer, "reconcile-order", encoded(r))
		if w.Code < 400 || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
			t.Fatal("Delivery-only reorder did not roll back atomically", event, w.Code, w.Body.String())
		}
		admissionSQL(t, f, `DROP TRIGGER fail_delivery_reconcile`)
	}
	q := configuredQueue(t, f)
	observed := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`)
	configured := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.configured'`)
	first := queueChangeHTTP(t, f, f.writer, "reconcile-order", "reorder", r)
	assertStoredQueueOrder(t, f, 2, []int64{3, 2, 1}, []int64{2, 3, 1})
	if first.State != "configured" || first.Revision != 2 || configuredQueue(t, f).Revision != q.Revision+1 || observed+1 != attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) || configured+1 != attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.configured'`) {
		t.Fatal("Delivery reconciliation did not advance both revisions exactly once", first)
	}
	state = projectionDigest(t, f, "") + enrollmentState(t, f)
	w := f.request("POST", route, f.writer, "stale-reconcile-order", encoded(r))
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("review_configuration_revision_conflict")) || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("Delivery-only write bypassed the configuration guard", w.Code, w.Body.String())
	}
	if replay := queueChangeHTTP(t, f, f.writer, "reconcile-order", "reorder", r); replay != first || state != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("Delivery-only replay changed state", replay)
	}
	projectionBefore := projectionDigest(t, f, "")
	configurationBefore := snapshotTable(t, f, "review_execution_config")
	eventsBefore := snapshotTable(t, f, "events")
	noop := queueChangeHTTP(t, f, f.writer, "reconciled-noop", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{3, 1}})
	if noop.State != "unchanged" || noop.Revision != 2 || projectionBefore != projectionDigest(t, f, "") || configurationBefore != snapshotTable(t, f, "review_execution_config") || eventsBefore != snapshotTable(t, f, "events") {
		t.Fatal("already reconciled selected orders were not a no-op", noop)
	}
}

func TestQueueReorderSelectedHiddenTargetsStayRetired(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical-%t", historical), func(t *testing.T) {
			f := queueEnrollmentFixture(t)
			configureQueue(t, f, 0, 1, 2, 3, 4)
			retireQueueOnly(t, f, 2)
			if historical {
				// Close both the current parent and retirement marker, retaining
				// only historical retirement as the ordinary enrollment guard.
				for i, retired := range [][]reviews.RetiredPull{{{Number: 2, Reason: "superseded"}}, {}} {
					q := configuredQueue(t, f)
					r := reviews.ObservationRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "replacement", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{projectionPull(1), projectionPull(3), projectionPull(4)}, Retired: retired}
					w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, fmt.Sprintf("hidden-reorder-replace-%d", i), encoded(r))
					if w.Code != 201 {
						t.Fatal(w.Code, w.Body.String())
					}
				}
			}
			retiredBefore := snapshotTable(t, f, "review_current_retired")
			parentsBefore := attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls WHERE project_id=? AND number=2`, attemptProject)
			queueChangeHTTP(t, f, f.writer, "reorder-hidden-selection", "reorder", QueueChange{ExpectedConfigurationRevision: 1, Order: []int64{4, 2, 1}})
			assertStoredQueueOrder(t, f, 2, []int64{4, 2, 3, 1}, []int64{4, 3, 1})
			if retiredBefore != snapshotTable(t, f, "review_current_retired") || parentsBefore != attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls WHERE project_id=? AND number=2`, attemptProject) {
				t.Fatal("reorder reactivated a hidden selected PR")
			}
			projectionBefore := projectionDigest(t, f, "")
			noop := queueChangeHTTP(t, f, f.writer, "hidden-selection-noop", "reorder", QueueChange{ExpectedConfigurationRevision: 2, Order: []int64{4, 2, 1}})
			if noop.State != "unchanged" || noop.Revision != 2 || projectionBefore != projectionDigest(t, f, "") {
				t.Fatal("hidden selection caused spurious publication", noop)
			}
			if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
