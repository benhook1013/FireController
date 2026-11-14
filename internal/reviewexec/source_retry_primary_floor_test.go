package reviewexec

import (
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http"
	"testing"
	"time"
)

// Contract with GitHubQuota.primaryReadGate: the local refusal is observed
// during this attempt, but its absolute floor came from an older provider read.
func TestSourceRetryLocalPrimaryFloorRetainsLaterAttempt(t *testing.T) {
	f, r, now := sourceFixture(t)
	providerObserved := now.Add(-20 * time.Second)
	floor := providerObserved.Add(65 * time.Second)
	a := f.start(t, attemptProject, r)
	err := PreparationFailureAt("source_resolution", &reviewprovider.Error{Code: "provider_rate_limited", Status: http.StatusTooManyRequests, RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: *now, NotBefore: floor}})
	ok, problem := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, err)
	if problem != nil || !ok {
		t.Fatal("local primary floor terminalized later attempt", ok, problem)
	}
	o := cliWaitRead(t, f, r.ID)
	if o.State != "queued" || o.AttemptID != "" || o.Wait == nil || o.Wait.SourceRetry == nil || o.Wait.SourceRetry.RetryNotBefore != floor.Format(time.RFC3339Nano) {
		t.Fatal("original intent/floor not retained", o)
	}
	if completionNoticeCount(t, f) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("local refusal created completion or reserved provider work")
	}
	if early, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || early.ID != "" {
		t.Fatal("floor bypass", early, err)
	}
	*now = floor.Add(time.Nanosecond)
	if next, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || next.ID == "" {
		t.Fatal("ordinary retry did not resume after unchanged floor", next, err)
	}
}
