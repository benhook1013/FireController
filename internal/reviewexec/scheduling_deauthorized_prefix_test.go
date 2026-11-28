package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

func TestAutomaticPrefixRetainsChildButDoesNotBlockIndependentSuccessorAfterDisable(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2, 4})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	first := schedulingLatest(t, f)
	if first.State != "selected" || first.OperationID == "" {
		t.Fatal("ordinary automatic child missing", first)
	}
	schedulingEnable(t, f, 1, false)
	schedulingEnable(t, f, 2, true)
	before := snapshotTable(t, f, "review_operations")
	var limit int
	var oldPending bool
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		limit, err = f.e.automaticHostedQueuedPrefixChannel(ctx, tx, attemptProject, c, "hosted")
		if err != nil {
			return err
		}
		oldPending, err = automaticHostedPendingPullChannel(ctx, tx, attemptProject, 2, "hosted")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if limit != 2 || !oldPending {
		t.Fatalf("old bound child still blocks unrelated queue: limit=%d retained same-PR pending=%v", limit, oldPending)
	}
	if snapshotTable(t, f, "review_operations") != before {
		t.Fatal("prefix inspection changed an existing child")
	}
	schedulingGenerate(t, f, true)
	later := schedulingLatest(t, f)
	if later.ID == first.ID {
		t.Fatal("independent later pull was not nominated")
	}
	selectionNext(t, f, &refreshTestReader{})
	later = selectionRead(t, f, later.ID)
	if later.State != "selected" || later.OperationID == "" || later.OperationID == first.OperationID {
		t.Fatal("later selection did not acquire its own child", later)
	}
	for _, pair := range []struct {
		id   string
		pull int64
	}{{first.OperationID, 2}, {later.OperationID, 4}} {
		var pull int64
		var state string
		if err := f.store.DB().QueryRow("SELECT pull_number,state FROM review_operations WHERE id=?", pair.id).Scan(&pull, &state); err != nil || pull != pair.pull || state != "queued" {
			t.Fatalf("child binding changed: want pull %d queued, got %d %s (%v)", pair.pull, pull, state, err)
		}
	}
}
