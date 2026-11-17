package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type cohortEngineReader struct {
	*sourceCITestReader
	enroll  func([]reviewprovider.CITarget)
	factory func(context.Context, []reviewprovider.CITarget, string) (*reviewprovider.CIGateCohort, error)
}

func (r *cohortEngineReader) NewCIGateCohort(ctx context.Context, targets []reviewprovider.CITarget, gate string) (*reviewprovider.CIGateCohort, error) {
	if r.factory != nil {
		return r.factory(ctx, targets, gate)
	}
	if r.enroll != nil {
		r.enroll(targets)
	}
	// Exercise orchestration and its safe ordinary-reader fallback, independently
	// of the provider implementation tested in reviewprovider.
	return nil, errors.New("synthetic unsupported cohort")
}

func TestLifecycleCohortPreparesBeforeCollectionAndReleasesReservation(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			var events []string
			r := &cohortEngineReader{sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				events = append(events, fmt.Sprintf("pull%d", n))
				return lifecyclePull(n), nil
			}}}}
			// Seed both metadata/enrichment lanes with completed exact comparisons.
			lifecycleTick(t, f, s, r)
			lifecycleTick(t, f, s, r)
			clock.Add(6 * time.Minute)
			events = nil
			r.enroll = func(targets []reviewprovider.CITarget) {
				events = append(events, "enroll")
				if len(targets) != 2 || targets[0].PullNumber == targets[1].PullNumber {
					t.Fatal("invalid enrollment", targets)
				}
			}
			r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				events = append(events, fmt.Sprintf("ci%d", target.PullNumber))
				if !s.lanes[lifecycleEnrichment].projects[attemptProject] {
					t.Fatal("reservation released during cohort")
				}
				if limited {
					return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryAfterSeconds: 120}
				}
				return sourceCITestObservation(target, "satisfied"), nil
			}
			readers := map[string]TargetReader{"test-capability": r}
			first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			if err = f.e.observeLifecycleCohort(t.Context(), *first, readers, s); err != nil {
				t.Fatal(err)
			}
			want := 5
			if limited {
				want = 4
			}
			if len(events) != want || events[0][:4] != "pull" || events[1][:4] != "pull" || events[2] != "enroll" || events[3][:2] != "ci" {
				t.Fatal("unexpected collection order", events)
			}
			if len(s.lanes[lifecycleEnrichment].projects) != 0 || len(s.lanes[lifecycleEnrichment].accounts) != 0 {
				t.Fatal("reservation leaked")
			}
			if limited && !clock.Now().Before(s.accounts[first.account].until) {
				t.Fatal("rate limit lost")
			}
		})
	}
}

func TestPeriodicCohortDueExcludesHintsAndMovement(t *testing.T) {
	now := time.Now()
	d := lifecycleDue{head: "h", baseSHA: "b", baseRef: "main", enrichmentHead: "h", enrichmentBaseSHA: "b", enrichmentBaseRef: "main", enrichmentNext: now, ci: now}
	if !periodicCohortDue(d, now) {
		t.Fatal("due periodic excluded")
	}
	moved := d
	moved.head = "new"
	if periodicCohortDue(moved, now) {
		t.Fatal("movement enrolled")
	}
	hinted := d
	hinted.hints[lifecycleEnrichment].wanted = 1
	if periodicCohortDue(hinted, now) {
		t.Fatal("hint enrolled")
	}
	future := d
	future.ci = now.Add(time.Second)
	if periodicCohortDue(future, now) {
		t.Fatal("future CI enrolled")
	}
}

func TestLifecycleCohortCancellationBeforeFirstMemberPreservesDue(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	configureLifecycleTwoPulls(t, f)
	r := &cohortEngineReader{sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }}}}
	lifecycleTick(t, f, s, r)
	lifecycleTick(t, f, s, r)
	clock.Add(6 * time.Minute)
	readers := map[string]TargetReader{"test-capability": r}
	first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	before2, before4 := s.due[lifecycleKey{attemptProject, 2}], s.due[lifecycleKey{attemptProject, 4}]
	beforeCalls := r.ciCalls.Load()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.factory = func(bound context.Context, targets []reviewprovider.CITarget, gate string) (*reviewprovider.CIGateCohort, error) {
		// The real constructor does not perform network I/O. Cancellation happens
		// after enrollment but before the engine could invoke any member.
		g, e := reviewprovider.NewGitHub("")
		if e != nil {
			t.Fatal(e)
		}
		cohort, e := g.NewCIGateCohort(bound, targets, gate)
		cancel()
		return cohort, e
	}
	if err = f.e.observeLifecycleCohort(ctx, *first, readers, s); err != nil {
		t.Fatal(err)
	}
	if r.ciCalls.Load() != beforeCalls {
		t.Fatal("cancelled cohort collected CI")
	}
	for n, before := range map[int64]lifecycleDue{2: before2, 4: before4} {
		after := s.due[lifecycleKey{attemptProject, n}]
		if !after.ci.Equal(before.ci) || !after.enrichmentNext.Equal(before.enrichmentNext) || after.enrichmentError != before.enrichmentError {
			t.Fatal("unobserved member falsely settled", n)
		}
	}
	if len(s.lanes[lifecycleEnrichment].projects) != 0 || len(s.lanes[lifecycleEnrichment].accounts) != 0 {
		t.Fatal("cancelled reservation leaked")
	}
	if clock.Now().Before(s.accounts[first.account].until) {
		t.Fatal("cancellation invented provider cooldown")
	}
}
