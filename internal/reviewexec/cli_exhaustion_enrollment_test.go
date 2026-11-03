package reviewexec

import (
	"strings"
	"testing"
)

func TestCLIExhaustionRetainsOriginalFactsAcrossNeighborEnrollment(t *testing.T) {
	for _, mode := range []string{"exhausted", "positive", "mode_changed", "paused", "removed", "positive_mode_changed", "positive_paused", "positive_removed"} {
		t.Run(mode, func(t *testing.T) {
			f, a, input := cliAllowanceFixture(t)
			previous := int64(1)
			configuration := Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
			if strings.TrimPrefix(mode, "positive_") == "mode_changed" {
				configuration.Mode = "stacked"
			}
			if strings.TrimPrefix(mode, "positive_") == "paused" {
				configuration.Paused = true
			}
			if strings.TrimPrefix(mode, "positive_") == "removed" {
				configuration.Queue = []int64{4, 6}
			}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "enroll-neighbor", configuration); err != nil {
				t.Fatal(err)
			}
			before := cliAllowanceState(t, f, a)
			journal := attemptState(t, f, a)
			positive := mode == "positive" || strings.HasPrefix(mode, "positive_")
			var err error
			if positive {
				err = f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input)
			} else {
				input.Remaining = 0
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, input)
			}
			if mode != "exhausted" && mode != "positive" {
				if err == nil || cliAllowanceState(t, f, a) != before {
					t.Fatal("retention bypassed a positive or current-binding guard", mode, err)
				}
				return
			}
			if err != nil {
				t.Fatal("neighbor enrollment discarded the original allowance observation", err)
			}
			record, _ := cliAllowanceRead(t, f, a)
			if record.ConfigurationRevision != 1 || record.OperationID != a.OperationID || record.AttemptID != a.ID || record.Observation != input || validCLIAllowance(record.Observation) != positive {
				t.Fatal("allowance lost its original attribution or positive/exhausted outcome", record)
			}
			if positive {
				if attemptState(t, f, a) != journal {
					t.Fatal("positive retention admitted/dispatched an action")
				}
				return
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
