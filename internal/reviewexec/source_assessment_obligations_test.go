package reviewexec

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceAssessmentsUnconfiguredCarriedObligations(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("configured=%t/pending=%t", configured, pending), func(t *testing.T) {
				f, a, _, reader, op := readinessFixture(t)
				if !configured {
					admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
				}
				var count int64
				reason := ""
				if pending {
					integrationImportHold(t, f, 2)
					count, reason = 1, "source_obligations_pending"
				}
				policyState, readinessState := "unconfigured", "unconfigured"
				var revision int64
				channels := []PolicyAssessmentChannel{}
				reasons := []string{"policy_unconfigured"}
				if configured {
					policyState, readinessState, revision = "sufficient", "ready_as_observed", 2
					channels = []PolicyAssessmentChannel{{Channel: "cli", Covered: true}}
					reasons = []string{}
					if pending {
						policyState, readinessState = "held", "held"
					}
				}
				if pending {
					reasons = append(reasons, reason)
				}
				before, calls, ciCalls, reviewCalls := readinessDurable(t, f, a), reader.calls.Load(), reader.ciCalls.Load(), reader.reviewCalls.Load()
				policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), sourceCIScope(t, f), 2)
				if err != nil || policy.State != policyState || policy.PolicyRevision != revision || policy.SourceObligationsPending != count || policy.Reason != reason || policy.RefreshID != op.ID || policy.Target != *op.Observation || !reflect.DeepEqual(policy.Channels, channels) || policy.IncomingRoutesPending != 0 || policy.IncomingRoutesCapacity {
					t.Error("source policy lost independent carried hold or policy shape", policy, err)
				}
				readiness, err := f.e.ReadSourceReadiness(t.Context(), sourceCIScope(t, f), 2)
				if err != nil || readiness.State != readinessState || readiness.PolicyRevision != revision || readiness.SourceObligationsPending != count || !reflect.DeepEqual(readiness.Reasons, reasons) || readiness.RefreshID != op.ID || readiness.Target != *op.Observation || !reflect.DeepEqual(readiness.Channels, channels) || readiness.IncomingRoutesPending != 0 || readiness.IncomingRoutesCapacity {
					t.Error("source readiness lost independent carried hold or policy shape", readiness, err)
				}
				if before != readinessDurable(t, f, a) || calls != reader.calls.Load() || ciCalls != reader.ciCalls.Load() || reviewCalls != reader.reviewCalls.Load() {
					t.Fatal("local source assessments mutated facts or called provider")
				}
			})
		}
	}
}

func TestSourceAssessmentsUnconfiguredObligationsFailClosed(t *testing.T) {
	f, a, _, reader, _ := readinessFixture(t)
	admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
	_, _, hold := integrationImportHold(t, f, 2)
	admissionSQL(t, f, `UPDATE review_adoption_obligations SET revision=2 WHERE id=?`, hold.ID)
	before, calls, ciCalls, reviewCalls := readinessDurable(t, f, a), reader.calls.Load(), reader.ciCalls.Load(), reader.reviewCalls.Load()
	policy, policyErr := f.e.ReadSourcePolicyAssessment(t.Context(), sourceCIScope(t, f), 2)
	readiness, readinessErr := f.e.ReadSourceReadiness(t.Context(), sourceCIScope(t, f), 2)
	t.Run("policy", func(t *testing.T) {
		attemptCode(t, policyErr, "storage_unavailable")
		if !reflect.DeepEqual(policy, SourcePolicyAssessment{}) {
			t.Fatal("invalid carried facts returned a partial policy assessment", policy)
		}
	})
	t.Run("readiness", func(t *testing.T) {
		attemptCode(t, readinessErr, "storage_unavailable")
		if !reflect.DeepEqual(readiness, SourceReadinessAssessment{}) {
			t.Fatal("invalid carried facts returned a partial readiness assessment", readiness)
		}
	})
	if before != readinessDurable(t, f, a) || calls != reader.calls.Load() || ciCalls != reader.ciCalls.Load() || reviewCalls != reader.reviewCalls.Load() {
		t.Fatal("failed source assessments mutated facts or called provider")
	}
}

func TestSourceAssessmentsUnconfiguredObligationsPreserveStaleHolds(t *testing.T) {
	for _, change := range []string{"configuration", "repository", "comparison"} {
		t.Run(change, func(t *testing.T) {
			f, a, _, reader, _ := readinessFixture(t)
			admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
			integrationImportHold(t, f, 2)
			reason := "source_configuration_changed"
			switch change {
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			case "comparison":
				reader.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("d", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
				}
				request := refreshRequest()
				request.HeadSHA = strings.Repeat("d", 40)
				queued := refreshEnqueue(t, f, "later-source", request)
				cancel, done := refreshStart(t, f, reader)
				refreshWait(t, f, queued.ID, "completed")
				refreshStop(t, cancel, done)
				reason = "source_comparison_changed"
			}
			readiness := readReadiness(t, f, reader, a, 2)
			if readiness.State != "held" || readiness.PolicyRevision != 0 || len(readiness.Channels) != 0 || readiness.SourceObligationsPending != 0 || !reflect.DeepEqual(readiness.Reasons, []string{reason}) {
				t.Fatal("stale source readiness evaluated inapplicable policy", readiness)
			}
			if change != "comparison" {
				policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), sourceCIScope(t, f), 2)
				if err != nil || policy.State != "held" || policy.Reason != reason || policy.PolicyRevision != 0 || len(policy.Channels) != 0 || policy.SourceObligationsPending != 0 {
					t.Fatal("stale source policy evaluated inapplicable obligations", policy, err)
				}
			}
		})
	}
}
