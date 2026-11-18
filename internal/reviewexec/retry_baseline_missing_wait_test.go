package reviewexec

import (
	"testing"
)

func TestRetryBaselineRefusesMissingWaitWithDetachedCooldown(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			if channel == "cli" {
				f, a, _, _ = cliWaitFixture(t)
				if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				f, a, _, _ = queuedCooldown(t)
			}
			baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, a.OperationID)
			if err != nil || baseline == nil {
				t.Fatal(baseline, err)
			}
			admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, a.OperationID)
			baseline, err = f.e.RetryBaseline(t.Context(), attemptProject, a.OperationID)
			if !isWaitUnavailable(err) || baseline != nil {
				t.Fatal("detached history lost comparison fence", baseline, err)
			}
		})
	}
}
func TestRetryBaselineFreshRequestHasNoPriorComparison(t *testing.T) {
	f := supervisorFixture(t)
	o := f.enqueue(t, attemptProject, "fresh-baseline", "cli", 2, 1)
	baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, o.ID)
	if err != nil || baseline != nil {
		t.Fatal(baseline, err)
	}
}
