package reviewexec

import (
	"context"
	"fmt"
	"testing"
)

func TestSupervisorTypedPreparationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, wantState, wantReason string
		refusal                     func() error
		reservations                int
	}{
		{"scheduling", "queued", "automatic_scheduling_unavailable", schedulingRecordUnavailable, 0},
		{"round-history", "queued", "automatic_scheduling_unavailable", roundUnavailable, 0},
		{"retained-history", "preparing", "", func() error {
			return automaticProjectStateFailure("storage_unavailable", "Synthetic retained history unavailable.", 503)
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := supervisorFixture(t)
			receipt := f.enqueue(t, attemptProject, "typed-refusal", "cli", 2, 1)
			driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
				return Preparation{}, fmt.Errorf("adapter context: %w", tc.refusal())
			}}
			if err := f.e.processNext(context.Background(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
				t.Fatal(err)
			}
			var state, reason, attemptID string
			if err := f.store.DB().QueryRow("SELECT state,reason,attempt_id FROM review_operations WHERE id=?", receipt.ID).Scan(&state, &reason, &attemptID); err != nil {
				t.Fatal(err)
			}
			if state != tc.wantState || reason != tc.wantReason || (attemptID != "") != (tc.reservations > 0) || driver.prepares.Load() != 1 || driver.calls.Load() != 0 ||
				attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != tc.reservations {
				t.Fatalf("state=%q reason=%q attempt=%q prepares=%d dispatches=%d", state, reason, attemptID, driver.prepares.Load(), driver.calls.Load())
			}
		})
	}
}
