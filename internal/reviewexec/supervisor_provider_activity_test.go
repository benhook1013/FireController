package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestSupervisorProviderActivityHoldsRetainOnlyQualifiedReasons(t *testing.T) {
	const private = "private-provider-diagnostic-must-not-be-retained"
	type refusalCase struct {
		name    string
		failure error
		reason  string
	}
	var cases []refusalCase
	for _, reason := range []string{
		"provider_activity_rate_limited", "provider_activity_unauthorized", "provider_activity_not_found", "provider_activity_response_invalid", "provider_activity_unavailable", "provider_activity_ambiguous", "provider_activity_state_unknown", "provider_activity_observation_invalid", "provider_activity_unproven", "provider_review_active",
	} {
		cases = append(cases, refusalCase{reason, &core.Error{Code: "review_held", Message: private, Details: map[string]string{"reason": reason, "provider_message": private, "retry_after": "123"}}, reason})
	}
	cases = append(cases,
		refusalCase{"unknown reason", &core.Error{Code: "review_held", Message: private, Details: map[string]string{"reason": "provider_activity_future_unqualified", "provider_message": private}}, "preparation_failed"},
		refusalCase{"unrelated code", &core.Error{Code: "unknown_provider_failure", Message: private, Details: map[string]string{"reason": "provider_activity_rate_limited", "provider_message": private}}, "preparation_failed"},
		refusalCase{"unqualified details", &core.Error{Code: "review_held", Message: private, Details: map[string]any{"reason": "provider_activity_rate_limited", "provider_message": private}}, "preparation_failed"},
		refusalCase{"raw diagnostic", errors.New("provider_activity_rate_limited " + private), "preparation_failed"},
	)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := preparationReason(test.failure); got != test.reason {
				t.Fatal("unqualified provider activity reason", got, test.reason)
			}
			// All classes share this persistence path. Two representative cases prove
			// the native held projection and accounting without repeating DB setup.
			if test.name != "provider_activity_rate_limited" && test.name != "raw diagnostic" {
				return
			}
			f := attemptProjectionFixture(t, true)
			roundGrant(t, f, 2, "hosted", 0, 0, 1, "one-round")
			receipt := f.enqueue(t, attemptProject, "activity-hold", "hosted", 2, 1)
			var original Attempt
			driver := &controlledDriver{prepare: func(_ context.Context, op Operation, _ Configuration, a Attempt) (Preparation, error) {
				if op.ID != receipt.ID || a.OperationID != receipt.ID || a.State != "preparing" || a.Revision != 1 {
					t.Fatal("activity refusal changed original preparing identity", op, a)
				}
				original = a
				return Preparation{}, test.failure
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			op := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), receipt.ID)
			if op.ID != receipt.ID || op.State != "held" || op.Reason != test.reason || op.Revision != 3 || op.AttemptID != original.ID || op.Wait != nil || op.Allowance != nil || op.ConfigurationRevision != 1 {
				t.Fatal("activity refusal lost original held operation", op)
			}
			attempt, err := readAttempt(t.Context(), f.store.DB(), attemptProject, original.ID)
			if err != nil || attempt.State != "held" || attempt.Revision != 2 || attempt.OperationID != receipt.ID || attempt.Anchor != nil || attempt.Intent != "" || attempt.ExternalID != "" {
				t.Fatal("activity refusal admitted provider work", attempt, err)
			}
			var transitionState, transitionReason, external string
			if err := f.store.DB().QueryRow(`SELECT state,reason,external_id FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, attempt.ID).Scan(&transitionState, &transitionReason, &external); err != nil || transitionState != "held" || transitionReason != test.reason || external != "" {
				t.Fatal("activity refusal lost qualified journal reason", transitionState, transitionReason, external, err)
			}
			if driver.prepares.Load() != 1 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, attempt.ID) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("activity refusal dispatched, retained capacity or claimed evidence")
			}
			_, projected := projectedAttempt(t, f, attempt)
			assertProjectedTimes(t, f, attempt, projected, true)
			if projected.State != "held" || projected.Reason != test.reason || projected.SourceRunID != "" || projected.TriggerID != "" {
				t.Fatal("native held projection lost qualified reason or invented credit", projected)
			}
			progress := readProgress(t, f, 2).Items[0].Channels[0]
			if progress.Pending != 0 || progress.Used != 0 || progress.Dry != 0 || progress.Minimum != 0 || progress.Maximum != 1 || progress.Tapered {
				t.Fatal("activity refusal changed round accounting", progress)
			}
			status, err := f.e.ReadOperationStatus(t.Context(), operationReadScope(t, f), receipt.ID)
			if err != nil || status.State != "held" || status.Reason != test.reason || status.Stage != "held" {
				t.Fatal("compact held status lost reason", status, err)
			}
			for _, table := range []string{"review_operations", "review_execution_transitions", "review_attempt_versions", "events", "receipts"} {
				if strings.Contains(snapshotTable(t, f, table), private) {
					t.Fatal("private provider diagnostic retained", table)
				}
			}
			wire, _ := json.Marshal(struct {
				Operation Operation
				Status    OperationStatus
			}{op, status})
			if strings.Contains(string(wire), private) || strings.Contains(string(wire), "retry_after") {
				t.Fatal("arbitrary provider details leaked to reads")
			}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
				t.Fatal("terminal activity hold retried provider work", err)
			}
		})
	}
}
