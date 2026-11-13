package reviewexec

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestLifecycleHintClaimRechecksCooldownAfterSourceRead(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "active"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			reader := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}
			readers := map[string]TargetReader{"test-capability": reader}
			lifecycleTick(t, f, s, reader)
			key := lifecycleKey{attemptProject, 2}
			due := s.due[key]
			due.enrichmentHead, due.enrichmentBaseSHA, due.enrichmentBaseRef = due.head, due.baseSHA, due.baseRef
			due.enrichmentNext = clock.Now().Add(time.Hour)
			s.due[key] = due
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("a", 40), CI: true})
			binding, err := f.e.ciWatchBinding(t.Context(), operationReadScope(t, f))
			if err != nil {
				t.Fatal(err)
			}
			initial := f.store.DB().Stats().WaitCount
			blocked := make(chan func(), 1)
			var once sync.Once
			// The first scheduler clock read follows project discovery. Occupy
			// the read pool then, so source selection blocks only after the
			// initial account check has passed and before pins are reserved.
			s.options.Now = func() time.Time {
				once.Do(func() { blocked <- holdCIWatchReadPool(t, f) })
				return clock.Now()
			}
			type reply struct {
				target *lifecycleTarget
				err    error
			}
			done := make(chan reply, 1)
			go func() {
				target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
				done <- reply{target, err}
			}()
			var release func()
			select {
			case release = <-blocked:
			case <-time.After(time.Second):
				t.Fatal("claim did not reach its first account check")
			}
			deadline := time.Now().Add(time.Second)
			for f.store.DB().Stats().WaitCount == initial && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if f.store.DB().Stats().WaitCount == initial {
				t.Fatal("claim did not pass its initial account check and block on source selection")
			}
			s.mu.Lock()
			s.finishAccountLocked(lifecycleTarget{account: binding.account, project: binding.project, lane: lifecycleMetadata}, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 60}, clock.Now())
			until := s.accounts[binding.account].until
			s.mu.Unlock()
			if expired {
				clock.Add(until.Sub(clock.Now()))
			}
			release()
			var got reply
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("claim did not finish after source read was released")
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if !expired {
				s.mu.Lock()
				hint := s.due[key].hints[lifecycleEnrichment]
				reserved := s.lanes[lifecycleEnrichment].accounts[binding.account] || s.lanes[lifecycleEnrichment].projects[binding.project]
				s.mu.Unlock()
				if got.target != nil || reserved || !hint.attemptAt.IsZero() || hint.wanted == hint.done {
					t.Fatal("new cooldown admitted work or consumed the CI hint", got.target, reserved, hint)
				}
				clock.Add(until.Sub(clock.Now()))
				got.target, got.err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			}
			if got.err != nil || got.target == nil || !got.target.forceCI || !got.target.ciOnly || got.target.hintGeneration != 1 || !s.due[key].hints[lifecycleEnrichment].attemptAt.Equal(clock.Now()) {
				t.Fatal("hint did not resume using current scheduler time at cooldown expiry", got.target, got.err)
			}
			if err := f.e.observeLifecycle(t.Context(), *got.target, s); err != nil {
				t.Fatal(err)
			}
		})
	}
}
