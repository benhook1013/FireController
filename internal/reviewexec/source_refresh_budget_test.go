package reviewexec

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceRefreshReservesFullCIGateBudgetAfterSource(t *testing.T) {
	for _, readiness := range []bool{false, true} {
		name := "ci"
		if readiness {
			name = "readiness"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAttemptFixture(t)
				f.configure(t, attemptProject, "test-capability", 0, false)
				request := sourceCIRequest()
				request.IncludeReadiness = readiness
				queued := refreshEnqueue(t, f, "budget-"+name, request)
				reader := newReadinessTestReader()
				var operationDeadline time.Time
				reader.repository = func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) != 15*time.Second {
						t.Error("source phase escaped15s", deadline)
					}
					time.Sleep(10 * time.Second)
					return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
				}
				reader.ci = func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
					var ok bool
					operationDeadline, ok = ctx.Deadline()
					want := 45 * time.Second
					if readiness {
						want = 60 * time.Second
					}
					if !ok || time.Until(operationDeadline) != want {
						t.Error("source consumed gate allowance", time.Until(operationDeadline), want)
					}
					select {
					case <-time.After(35 * time.Second):
					case <-ctx.Done():
						return reviewprovider.CIObservation{}, ctx.Err()
					}
					return sourceCITestObservation(target, "satisfied"), nil
				}
				reader.review = func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
					deadline, ok := ctx.Deadline()
					if !ok || !deadline.Equal(operationDeadline) || time.Until(deadline) != 25*time.Second {
						t.Error("readiness did not retain aggregate budget")
					}
					select {
					case <-time.After(10 * time.Second):
					case <-ctx.Done():
						return reviewprovider.PullReviewObservation{}, ctx.Err()
					}
					return reviewprovider.PullReviewObservation{Target: target, State: "complete", ObservedAt: time.Now().UTC(), PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Threads: []reviewprovider.PullReviewThread{}}, nil
				}
				worked, err := f.e.refreshNextScheduled(t.Context(), map[string]TargetReader{"test-capability": reader}, newSourceRefreshSchedule())
				if err != nil || !worked {
					t.Fatal(worked, err)
				}
				got := refreshRead(t, f, queued.ID)
				if got.State != "completed" || got.CI == nil || got.CI.State != "satisfied" {
					t.Fatal("slow successful gate lost", got)
				}
				if readiness && (got.ReviewState == nil || got.ReviewState.State != "complete") {
					t.Fatal("readiness phase lost", got)
				}
			})
		})
	}
}
