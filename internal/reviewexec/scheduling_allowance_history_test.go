package reviewexec

import "testing"

func TestSchedulingDetachedCLIAllowanceRemainsValidHistory(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_scheduling_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "scheduling-allowance", "cli", 2, 1))
			in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 3, Limit: 5, FullCapacityInMS: 12000, RollingWindowMS: 3600000, ChangedFiles: 2}
			if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
				t.Fatal(err)
			}
			held, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
			if err != nil {
				t.Fatal(err)
			}
			o, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, a.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if o.AttemptID != "" {
				t.Fatal("attempt not detached", o)
			}
			if _, err := readCLIAllowanceRecord(t.Context(), f.store.DB(), o, held); err != nil {
				t.Fatal("valid detached allowance rejected", err)
			}
			assertCLIWaitSnapshot(t, f)
			assertCLIWaitNoCredit(t, f)
		})
	}
}
