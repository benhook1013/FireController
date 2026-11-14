package reviewexec

import "testing"

func TestCLIExhaustionPauseRetainsNegativeNotPositiveAllowance(t *testing.T) {
	for _, zero := range []bool{true, false} {
		t.Run(map[bool]string{true: "negative", false: "positive"}[zero], func(t *testing.T) {
			f, a, in := cliAllowanceFixture(t)
			f.configure(t, attemptProject, "test-capability", 1, true)
			before := attemptState(t, f, a)
			if zero {
				in.Remaining = 0
				if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
					t.Fatal(err)
				}
				record, _ := cliAllowanceRead(t, f, a)
				if record.Observation != in || record.ConfigurationRevision != 1 || attemptState(t, f, a) != before {
					t.Fatal("negative observation changed original attempt or binding", record)
				}
				if _, err := f.e.persistCooldown(t.Context(), cliWaitRead(t, f, a.OperationID), a); err != nil {
					t.Fatal("paused exhaustion lost queued cooldown", err)
				}
				op := cliWaitRead(t, f, a.OperationID)
				if op.State != "queued" || op.Wait == nil || op.Wait.BaselineAttemptID != a.ID {
					t.Fatal("original cooldown identity lost", op)
				}
			} else {
				if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err == nil {
					t.Fatal("positive allowance crossed project pause")
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != 0 || attemptState(t, f, a) != before {
					t.Fatal("positive refusal changed attempt")
				}
			}
		})
	}
}
