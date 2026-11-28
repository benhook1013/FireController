package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestPreparedCLIAdmissionRefusalSettlement(t *testing.T) {
	for _, scenario := range []string{"policy", "scheduling", "absent", "exhausted", "different-comparison", "publication-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, input := cliAllowanceFixture(t)
			anchor := input.Anchor
			switch scenario {
			case "absent":
			case "exhausted":
				input.Remaining = 0
				if err := f.e.RecordCLIExhaustion(t.Context(), a.ProjectID, a.ID, a.Revision, input); err != nil {
					t.Fatal(err)
				}
			default:
				if err := f.e.RecordCLIAllowance(t.Context(), a.ProjectID, a.ID, a.Revision, input); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "different-comparison" {
				anchor.PatchSHA256 = strings.Repeat("9", 64)
			}
			refusal := newStoredUnavailable(storedPolicyHistory, "Policy history unavailable in fixture.")
			if scenario == "scheduling" {
				refusal = schedulingUnavailable()
			}
			publicationFailure := errors.New("attempt projection unavailable in fixture")
			if scenario == "publication-failure" {
				f.e.publishAttempt = func(context.Context, *sql.Tx, Attempt) error {
					return publicationFailure
				}
			}
			before := cliAllowanceState(t, f, a)
			original := roundOperation(t, f, a.OperationID)
			var settled Attempt
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				op, err := readOperationBase(ctx, tx, a.ProjectID, a.OperationID)
				if err != nil {
					return err
				}
				settled, err = f.e.settleCLIAdmissionRefusal(ctx, tx, op, a, anchor, refusal)
				if scenario == "publication-failure" && !errors.Is(err, publicationFailure) {
					t.Errorf("expected attempt publication to fail, got %v", err)
				}
				return err
			})
			if scenario != "policy" && scenario != "scheduling" {
				if err == nil || scenario != "publication-failure" && !errors.Is(err, refusal) {
					t.Fatalf("expected settlement to be refused, got %v", err)
				}
				if after := cliAllowanceState(t, f, a); after != before {
					t.Fatal("refused settlement changed the journal, allowance, or reservation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			queued := roundOperation(t, f, a.OperationID)
			if settled.ID != a.ID || settled.Revision != 2 || settled.State != "held" ||
				queued.ID != original.ID || queued.Revision != original.Revision+1 || queued.State != "queued" ||
				queued.AttemptID != "" || queued.Reason != "automatic_scheduling_unavailable" {
				t.Fatalf("expected the original request to be queued with its unused attempt held, got %+v and %+v", queued, settled)
			}
			if attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
				t.Fatal("unused attempt retained its reservation")
			}
			assertCLIWaitSnapshot(t, f)
			assertCLIWaitNoCredit(t, f)
		})
	}
}
