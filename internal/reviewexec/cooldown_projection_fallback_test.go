package reviewexec

import (
	"context"
	"strings"
	"testing"
)

func TestNativeProjectionUnusableHostedCooldownHoldsOnlyItsOperation(t *testing.T) {
	for _, scenario := range []string{"absent", "malformed", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			receipt := f.enqueue(t, attemptProject, "unusable-hosted-cooldown", "hosted", 2, 1)
			body := "{}"
			if scenario == "oversized" {
				body = strings.Repeat("x", 16385)
			}
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				if o.Channel != "hosted" {
					return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
				}
				if scenario != "absent" {
					if _, err := f.store.DB().ExecContext(ctx, `INSERT INTO review_hosted_preflight(attempt_id,project_id,body_json) VALUES(?,?,?)`, a.ID, o.ProjectID, body); err != nil {
						return Preparation{}, err
					}
				}
				return Preparation{}, sourceHold("hosted_allowance_cooldown")
			}}
			drivers := map[string]Driver{"test-capability": driver, "other-account": driver}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal("unusable Hosted evidence stopped unrelated work", err)
			}
			var state, reason string
			if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_operations WHERE id=?`, receipt.ID).Scan(&state, &reason); err != nil || state != "held" || reason != "hosted_allowance_cooldown" {
				t.Fatal("cooldown refusal did not retain local hold", state, reason, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operation_waits WHERE baseline_attempt_id IS NOT NULL`) != 0 || driver.calls.Load() != 0 {
				t.Fatal("unproven cooldown retained capacity, invented timer, or dispatched")
			}
			if scenario != "absent" {
				var retained string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight`).Scan(&retained); err != nil || retained != body {
					t.Fatal("local hold rewrote malformed retained evidence", err)
				}
			}
			if scenario != "absent" {
				_, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), receipt.ID)
				attemptCode(t, err, "storage_unavailable")
			}
			var attemptID string
			if err := f.store.DB().QueryRow(`SELECT attempt_id FROM review_operations WHERE id=?`, receipt.ID).Scan(&attemptID); err != nil {
				t.Fatal(err)
			}
			_, projected := projectedAttempt(t, f, Attempt{ID: attemptID})
			if projected.State != "held" {
				t.Fatal("local hold not projected", projected.State)
			}
			other := f.enqueue(t, otherAttemptProject, "healthy-after-hosted-hold", "cli", 2, 1)
			if err := f.e.processNext(t.Context(), drivers); err != nil || supervisorOperationState(t, f, other.ID) != "completed" || driver.calls.Load() != 1 {
				t.Fatal("another project did not continue after local hold", err)
			}
		})
	}
}
