package reviewexec

import (
	"github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"testing"
)

func TestAttemptProjectionReasonRetainsOnlySafeDiagnosticStates(t *testing.T) {
	for _, state := range []string{"held", "failed", "rate_limited", "uncertain", "cancelled", "completed"} {
		t.Run(state, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "reason-native", "cli", 2, 1))
			_, preparing := projectedAttempt(t, f, a)
			if preparing.Reason != "" {
				t.Fatal("preparing filler reason")
			}
			var err error
			if state == "uncertain" || state == "completed" {
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, state, "hosted_finished_inventory_unproven", "")
			if err != nil {
				t.Fatal(err)
			}
			pull, got := projectedAttempt(t, f, a)
			want := "hosted_finished_inventory_unproven"
			if state == "cancelled" || state == "completed" {
				want = ""
			}
			if got.Reason != want || len(pull.Pull.LatestAttempts) != 1 || pull.Pull.LatestAttempts[0].Reason != want {
				t.Fatal("journal/summary reason mismatch", got, pull.Pull.LatestAttempts)
			}
			old := projectionDigest(t, f, pull.SnapshotID)
			if _, err = f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); err != nil || old != projectionDigest(t, f, pull.SnapshotID) {
				t.Fatal("exact projection replay changed history", err)
			}
		})
	}
}
func TestAttemptProjectionUnsafeOptionalReasonDoesNotBlockSettlement(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "unsafe-optional-reason", "cli", 2, 1))
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "unsafe /private provider text", ""); err == nil {
		t.Fatal("mandatory journal reason validation relaxed")
	}
	a, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "UPPER_CODE", "")
	if err != nil {
		t.Fatal("unsafe optional reason blocked journal settlement", err)
	}
	_, got := projectedAttempt(t, f, a)
	if got.Reason != "" {
		t.Fatal("unsafe reason exposed", got)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("diagnostic changed reservation/evidence semantics")
	}
	var body reviews.AttemptsResponse
	raw := projectionRead(t, f, "/pulls/2/attempts", &body)
	if strings.Contains(string(raw), "UPPER_CODE") {
		t.Fatal("provider prose leaked")
	}
}
