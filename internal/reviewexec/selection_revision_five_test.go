package reviewexec

import (
	"context"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// An interruption before the first completed hold does not move the project's
// scheduling revision. The original selection still owns its bounded retry.
func TestSelectionRevisionFiveHoldKeepsExactSchedulingAuthority(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	original := schedulingLatest(t, f)
	recovery0030Claim(t, f, original.ID)
	if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	hold := selectionRead(t, f, original.ID)
	if hold.Revision != 5 || hold.State != "held" || hold.Reason != "source_fetch_timeout" || hold.OperationID != "" {
		t.Fatalf("expected original completed hold at selection revision 5: %+v", hold)
	}

	ageSourceRetry(t, f, original.ID)
	schedulingGenerate(t, f, true)
	after := selectionRead(t, f, original.ID)
	if after.Revision != 6 || after.State != "queued" || after.OperationID != "" {
		t.Fatalf("expected same original request to be queued: %+v", after)
	}
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, original.ID)
	if got.State != "selected" || got.OperationID == "" || got.ActorID != original.ActorID || got.WorkerID != original.WorkerID ||
		attemptCount(t, f.store, "SELECT count(*) FROM review_selections") != 1 ||
		attemptCount(t, f.store, "SELECT count(*) FROM review_operations") != 1 ||
		attemptCount(t, f.store, "SELECT count(*) FROM review_execution_attempts") != 0 {
		t.Fatalf("original request not selected exactly once without provider dispatch: %+v", got)
	}
}
