package reviewexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestLifecycleCIHintSurvivesMetadataPinConflict(t *testing.T) {
	for _, phase := range []string{"before_ci", "before_publication"} {
		t.Run(phase, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			pull := lifecyclePull(2)
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }}}
			readers := map[string]TargetReader{"test-capability": r}
			lifecycleTick(t, f, s, r)
			key := lifecycleKey{attemptProject, 2}
			initial := s.due[key]
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, HeadSHA: pull.HeadSHA, CI: true})
			target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || target == nil || !target.ciOnly || !target.forceCI {
				t.Fatal(target, err)
			}
			publishMetadata := func() {
				hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true})
				metadata, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
				if err != nil || metadata == nil {
					t.Fatal(metadata, err)
				}
				p := pull
				p.Title = "Changed title, identical comparison"
				mr := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return p, nil }}
				metadata.reader = mr
				if err = f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "before_ci" {
				publishMetadata()
			} else {
				r.ci = func(_ context.Context, ct reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
					publishMetadata()
					return sourceCITestObservation(ct, "failed"), nil
				}
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			due := s.due[key]
			h := due.hints[lifecycleEnrichment]
			if h.wanted == h.done || !h.attemptAt.Equal(clock.Now()) || !due.ci.Equal(initial.ci) {
				t.Fatal("conflict consumed or unthrottled CI hint", h, due.ci)
			}
			if got, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || got != nil {
				t.Fatal("retry bypassed five-second floor", got, err)
			}
			clock.Add(5 * time.Second)
			target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || target == nil || !target.forceCI {
				t.Fatal("hint did not retry before CI interval", target, err)
			}
			r.ci = func(_ context.Context, ct reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(ct, "failed"), nil
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			if got := lifecycleRead(t, f); got.CIState != "failure" {
				t.Fatal("failed CI remained hidden", got.CIState)
			}
			h = s.due[key].hints[lifecycleEnrichment]
			if h.done != h.wanted {
				t.Fatal("successful retry did not consume hint", h)
			}
		})
	}
}

func TestLifecycleWebhookAdmissionPrecedesWatchDemand(t *testing.T) {
	for _, failure := range []string{"database", "capacity", "no_enrolled_match"} {
		t.Run(failure, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			s.readers = map[string]TargetReader{"test-capability": &ciWatchReader{}}
			scope := operationReadScope(t, f)
			binding, err := f.e.ciWatchBinding(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.unregisterCIWatch(sub)
			hint := githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("a", 40), CI: true}
			ctx := t.Context()
			if failure == "database" || failure == "no_enrolled_match" {
				if failure == "no_enrolled_match" {
					hint.HeadSHA = ""
					hint.PullNumbers = []int64{42}
					hint.Metadata = true
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				for n := range 64 * 100 {
					s.due[lifecycleKey{"other", int64(n + 1)}] = lifecycleDue{}
				}
			}
			if err = f.e.HintGitHub(ctx, hint); !lifecycleHintOverloaded(err) {
				t.Fatal("failed admission accepted", err)
			}
			if sub.entries[0].wanted != 0 || sub.entries[0].metadataWanted != 0 {
				t.Fatal("rejected delivery created watch demand")
			}
			if failure == "capacity" {
				clear(s.due)
			}
			if err = f.e.HintGitHub(t.Context(), hint); err != nil {
				t.Fatal(err)
			}
			if sub.entries[0].wanted != 1 {
				t.Fatal("retry did not admit exactly one watch hint", sub.entries[0].wanted)
			}
		})
	}
}
