package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"testing"
	"time"
)

type metadataQualificationReader struct {
	*sourceCITestReader
	supported bool
	qualify   func(context.Context, [2]int64) (reviewprovider.MetadataPairQualification, error)
	t         *testing.T
}

func (r *metadataQualificationReader) SupportsMetadataQualification() bool { return r.supported }
func (r *metadataQualificationReader) QualifyMetadataPair(ctx context.Context, id int64, repo string, numbers [2]int64) (reviewprovider.MetadataPairQualification, error) {
	if id != 801 || repo != "owner/repo" {
		r.t.Fatal("wrong configured target", id, repo)
	}
	return r.qualify(ctx, numbers)
}
func (r *metadataQualificationReader) PullMetadataPair(context.Context, int64, string, [2]int64) [2]reviewprovider.PullMetadataPairResult {
	r.t.Fatal("qualification must not activate batched publication")
	return [2]reviewprovider.PullMetadataPairResult{}
}
func TestLifecycleMetadataQualificationPreservesOrdinaryPublication(t *testing.T) {
	for _, mode := range []string{"matched", "inconclusive", "disabled", "unsupported", "hint", "watch", "late_hint", "probe_limit", "ordinary_limit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, ordinary := 0, 0
			r := &metadataQualificationReader{t: t, supported: mode != "unsupported", sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				ordinary++
				if mode == "ordinary_limit" {
					return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 17}
				}
				return lifecyclePull(n), nil
			}}}}
			r.qualify = func(_ context.Context, numbers [2]int64) (reviewprovider.MetadataPairQualification, error) {
				calls++
				if !s.lanes[lifecycleMetadata].projects[attemptProject] || len(s.lanes[lifecycleMetadata].accounts) != 1 {
					t.Fatal("missing account/project reservation")
				}
				result := reviewprovider.MetadataPairQualification{State: "matched", Requests: 5}
				if mode == "inconclusive" {
					result.State = "inconclusive"
				}
				if mode == "late_hint" {
					key := lifecycleKey{attemptProject, numbers[0]}
					d := s.due[key]
					d.hints[lifecycleMetadata].wanted++
					s.due[key] = d
				}
				if mode == "probe_limit" {
					return result, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 17}
				}
				if mode == "cancel" {
					cancel()
					return result, context.Canceled
				}
				return result, nil
			}
			s.options.QualifyMetadataPair = mode != "disabled"
			if mode == "hint" {
				s.due[lifecycleKey{attemptProject, 2}] = lifecycleDue{hints: [2]lifecycleHint{{wanted: 1}}}
			}
			if mode == "watch" {
				key := ciWatchKey{binding: ciWatchBinding{project: attemptProject}, number: 2}
				s.watches = make(map[ciWatchKey]*ciWatchEntry)
				s.watches[key] = &ciWatchEntry{owners: map[*ciWatchSubscription]bool{&ciWatchSubscription{}: true}}
			}
			readers := map[string]TargetReader{"test-capability": r}
			first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			if err = f.e.observeLifecycleCohort(ctx, *first, readers, s); err != nil {
				t.Fatal(err)
			}
			if len(s.lanes[lifecycleMetadata].projects) != 0 || len(s.lanes[lifecycleMetadata].accounts) != 0 {
				t.Fatal("reservation leak")
			}
			wantProbe, wantOrdinary := 1, 2
			switch mode {
			case "disabled", "unsupported", "hint":
				wantProbe, wantOrdinary = 0, 1
			case "watch":
				wantProbe, wantOrdinary = 0, 2
			case "probe_limit", "cancel":
				wantOrdinary = 0
			case "ordinary_limit":
				wantOrdinary = 1
			}
			if calls != wantProbe || ordinary != wantOrdinary {
				t.Fatal("diagnostic/ordinary counts", calls, ordinary, wantProbe, wantOrdinary)
			}
			if mode == "probe_limit" || mode == "ordinary_limit" {
				if !s.accounts[first.account].until.After(clock.Now()) {
					t.Fatal("provider cooldown lost")
				}
			}
			if mode == "probe_limit" || mode == "cancel" {
				for _, n := range []int64{2, 4} {
					d := s.due[lifecycleKey{attemptProject, n}]
					if !d.next.IsZero() || !d.checkedAt.IsZero() || !d.successAt.IsZero() || !d.ci.IsZero() {
						t.Fatal("probe fabricated publication", n, d)
					}
				}
			}
			if mode == "matched" || mode == "inconclusive" || mode == "late_hint" {
				for _, n := range []int64{2, 4} {
					d := s.due[lifecycleKey{attemptProject, n}]
					if d.head != lifecyclePull(n).HeadSHA || !d.next.Equal(clock.Now().Add(time.Minute)) || !d.ci.IsZero() {
						t.Fatal("ordinary metadata or CI freshness changed", n, d)
					}
				}
				if mode == "late_hint" {
					h := s.due[lifecycleKey{attemptProject, first.number}].hints[lifecycleMetadata]
					if h.wanted == h.done {
						t.Fatal("late hint consumed")
					}
				}
				clock.Add(2 * time.Minute)
				next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
				if err != nil || next == nil {
					t.Fatal(next, err)
				}
				if err = f.e.observeLifecycleCohort(t.Context(), *next, readers, s); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || ordinary != 3 {
					t.Fatal("one-shot repeated or ordinary work skipped", calls, ordinary)
				}
			}
		})
	}
}

// Cohort error accounting must not release a reservation that its enclosing
// deferred release still owns, even for errors that impose no cooldown.
func TestLifecycleMetadataQualificationErrorKeepsCohortReservation(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	configureLifecycleTwoPulls(t, f)
	r := &metadataQualificationReader{t: t, supported: true, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }}}}
	target, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleMetadata)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	target.cohortOwned = true
	s.mu.Lock()
	s.finishAccountLocked(*target, &reviewprovider.Error{Code: "provider_invalid_response"}, clock.Now())
	held := s.lanes[lifecycleMetadata].projects[target.project] && s.lanes[lifecycleMetadata].accounts[target.account]
	s.mu.Unlock()
	if !held {
		t.Fatal("cohort accounting released enclosing reservation")
	}
}
