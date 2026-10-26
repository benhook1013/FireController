package reviewexec

import "testing"

func TestCLIExhaustionRetainsOriginalFactsAcrossNeighborEnrollment(t *testing.T) {
	for _, mode := range []string{"exhausted", "positive", "mode_changed", "paused", "removed"} {
		t.Run(mode, func(t *testing.T) {
			f, a, input := cliAllowanceFixture(t)
			previous := int64(1)
			configuration := Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
			if mode == "mode_changed" {
				configuration.Mode = "stacked"
			}
			if mode == "paused" {
				configuration.Paused = true
			}
			if mode == "removed" {
				configuration.Queue = []int64{4, 6}
			}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "enroll-neighbor", configuration); err != nil {
				t.Fatal(err)
			}
			before := cliAllowanceState(t, f, a)
			var err error
			if mode == "positive" {
				err = f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input)
			} else {
				input.Remaining = 0
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, input)
			}
			if mode != "exhausted" {
				if err == nil || cliAllowanceState(t, f, a) != before {
					t.Fatal("retention bypassed a positive or current-binding guard", mode, err)
				}
				return
			}
			if err != nil {
				t.Fatal("neighbor enrollment discarded a zero observation", err)
			}
			record, _ := cliAllowanceRead(t, f, a)
			if record.ConfigurationRevision != 1 || record.OperationID != a.OperationID || record.AttemptID != a.ID || record.Observation != input || validCLIAllowance(record.Observation) {
				t.Fatal("negative facts rebound or became a permit", record)
			}
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "allowance_exhausted", ""); err != nil {
				t.Fatal(err)
			}
			op := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
			if op.ConfigurationRevision != 1 || op.State != "held" || op.Allowance == nil || op.Allowance.Kind != "cli_exhausted" || op.Allowance.Remaining == nil || *op.Allowance.Remaining != 0 {
				t.Fatal("held read lost original quota observation", op)
			}
		})
	}
}
