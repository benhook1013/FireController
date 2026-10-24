package reviewexec

import (
	"context"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceCICandidateDiagnosticRetainedWithoutCreditOrRecollection(t *testing.T) {
	for _, reason := range []string{"ci_merge_identity_unproven", "ci_merge_pending"} {
		t.Run(reason, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				now := time.Now().UTC()
				return reviewprovider.CIObservation{Target: target, StartedAt: now, ObservedAt: now, State: "unproven", Reason: reason, MergeState: "unproven", Candidate: &reviewprovider.CICandidate{State: "unproven", Reason: reason, RepositoryID: target.RepositoryID, Repository: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, MergeState: "unproven", ObservedAt: now}}, nil
			}}
			receipt := refreshEnqueue(t, f, "candidate-reason", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			if got.Observation == nil || got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != reason || got.CI.Required != 0 || got.CI.Satisfied != 0 {
				t.Fatal("candidate reason lost or unverified CI credited", got)
			}
			before := sourceCITestState(t, f)
			if readSourceCI(t, f).Reason != reason || refreshRead(t, f, receipt.ID).CI.Reason != reason || refreshEnqueue(t, f, "candidate-reason", sourceCIRequest()) != receipt || sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
				t.Fatal("local reads/replay changed evidence or recollected provider")
			}
		})
	}
}
