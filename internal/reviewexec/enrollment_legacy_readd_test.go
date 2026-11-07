package reviewexec

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestEnrollmentReaddPreservesRetainedLegacyCoverage(t *testing.T) {
	for _, removed := range []string{"pull", "channel"} {
		for _, finalized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/finalized=%t", removed, finalized), func(t *testing.T) {
				// The predecessor fixture has an authentic configuration receipt and
				// real native capture, but predates automatic round enrollment.
				f, original, evidence, plan := coverageFixture(t, 0, false)
				adjudicationComplete(t, f, original)
				if finalized {
					roundFinalize(t, f, original, evidence, 0, false)
				}
				assessmentPolicy(t, f, 1, []string{"cli"})
				reader := newReadinessTestReader()
				collectReadiness(t, f, reader, "legacy-readiness", 2, 1)
				operation := roundOperation(t, f, original.OperationID)
				adjudication := adjudicationRead(t, f, evidence)
				if operation.State != "completed" || roundRead(t, f, 2, "cli").Configured {
					t.Fatal("fixture did not establish unconfigured completed native history")
				}

				scope := f.scope(t, attemptProject, f.admin, true)
				remove := enrollmentConfiguration(1, []int64{4}, []string{"cli", "hosted"})
				if removed == "channel" {
					remove = enrollmentConfiguration(1, []int64{2, 4}, []string{"hosted"})
				}
				if _, err := f.e.Configure(t.Context(), scope, "remove-legacy-pair", remove); err != nil {
					t.Fatal(err)
				}
				readd := enrollmentConfiguration(2, []int64{2, 4, 6}, []string{"cli", "hosted"})
				before := enrollmentState(t, f)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.writer, false), "readd-legacy-pair", readd)
				attemptCode(t, err, "forbidden")
				if enrollmentState(t, f) != before {
					t.Fatal("unauthorized re-enrollment changed configuration or round history")
				}
				receipt, err := f.e.Configure(t.Context(), scope, "readd-legacy-pair", readd)
				if err != nil {
					t.Fatal(err)
				}
				if got := roundRead(t, f, 2, "cli"); got.Configured {
					t.Errorf("re-enrollment allocated new rounds over retained legacy history: %+v", got)
				}
				// Retained work in one pair must not suppress unrelated new pairs.
				if removed == "pull" {
					assertEnrollmentDefault(t, f, 2, "hosted")
				} else {
					assertEnrollmentDefault(t, f, 4, "cli")
				}
				assertEnrollmentDefault(t, f, 6, "cli")
				assertEnrollmentDefault(t, f, 6, "hosted")
				if !reflect.DeepEqual(operation, roundOperation(t, f, original.OperationID)) || !reflect.DeepEqual(adjudication, adjudicationRead(t, f, evidence)) || attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings WHERE operation_id=?`, original.OperationID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions WHERE operation_id=?`, original.OperationID) != 0 {
					t.Fatal("re-enrollment rewrote legacy native evidence or awarded round credit")
				}
				before = enrollmentState(t, f)
				replay, err := f.e.Configure(t.Context(), scope, "readd-legacy-pair", readd)
				if err != nil || !reflect.DeepEqual(receipt, replay) || enrollmentState(t, f) != before {
					t.Fatal("re-enrollment replay changed retained allocations", err)
				}

				assessment, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), evidence)
				wantPolicy, wantReadiness := "sufficient", "ready_as_observed"
				if !finalized {
					wantPolicy, wantReadiness = "held", "held"
				}
				if err != nil || assessment.State != wantPolicy || len(assessment.Channels) != 1 || assessment.Channels[0].RoundsConfigured || assessment.Channels[0].Covered != finalized || assessment.Channels[0].Pending == finalized {
					t.Errorf("re-enrollment changed legacy policy coverage: %+v, %v", assessment, err)
				}
				readiness := readReadiness(t, f, reader, original, 2)
				if readiness.State != wantReadiness || slices.Contains(readiness.Reasons, "review_rounds_pending") || !finalized && !slices.Contains(readiness.Reasons, "review_evidence_pending") {
					t.Errorf("re-enrollment changed legacy readiness: %+v", readiness)
				}
				if !finalized {
					before = enrollmentState(t, f)
					revision, minimum, maximum := int64(0), int64(1), int64(1)
					_, err := f.e.ConfigureRoundControl(t.Context(), scope, "grant-before-legacy-finalization", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
					attemptCode(t, err, "review_round_inflight")
					if enrollmentState(t, f) != before {
						t.Fatal("unbound pending grant refusal changed state")
					}
					roundFinalize(t, f, original, evidence, 0, false)
					if got := readReadiness(t, f, reader, original, 2); got.State != "ready_as_observed" {
						t.Fatal("finalization did not recover retained legacy readiness", got)
					}
				}
				if complete, err := f.e.DiscoveryComplete(t.Context(), attemptProject, plan, "cli"); err != nil || !complete {
					t.Errorf("re-enrollment reopened completed legacy discovery: %v, %v", complete, err)
				}
				next := f.start(t, attemptProject, f.enqueue(t, attemptProject, "after-readd", "cli", 2, 3))
				anchor := attemptAnchor(attemptProject, 2)
				anchor.ParentSHA = anchor.BaseSHA
				next, err = f.e.Admit(t.Context(), attemptProject, next.ID, next.Revision, anchor, "")
				if err != nil {
					t.Fatal(err)
				}
				before = adjudicationState(t, f, next)
				err = f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision)
				attemptCode(t, err, "review_held")
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Details.(map[string]string)["reason"] != "target_already_covered" || adjudicationState(t, f, next) != before {
					t.Fatal("re-enrollment lost the completed native action guard", err)
				}
				roundSnapshotValid(t, f)
			})
		}
	}
}

func TestEnrollmentRetainedOperationsDoNotSuppressOtherProjects(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	f.enqueue(t, otherAttemptProject, "other-project-legacy-work", "cli", 2, 1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "new-project-enrollment", enrollmentConfiguration(0, []int64{2}, []string{"cli", "hosted"})); err != nil {
		t.Fatal(err)
	}
	assertEnrollmentDefault(t, f, 2, "cli")
	assertEnrollmentDefault(t, f, 2, "hosted")
	roundSnapshotValid(t, f)
}
