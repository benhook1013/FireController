package reviewexec

import (
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchClaimRechecksCooldownAfterAuthorization(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "active"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			f, s, clock, reader, scope := ciWatchFixture(t)
			binding, err := f.e.ciWatchBinding(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.unregisterCIWatch(sub)
			release := holdCIWatchReadPool(t, f)
			initial := f.store.DB().Stats().WaitCount
			type reply struct {
				target *lifecycleTarget
				err    error
			}
			done := make(chan reply, 1)
			go func() { target, err := f.e.claimCIWatch(t.Context(), s.readers, s); done <- reply{target, err} }()
			deadline := time.Now().Add(time.Second)
			for f.store.DB().Stats().WaitCount == initial && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if f.store.DB().Stats().WaitCount == initial {
				t.Fatal("claim did not reach blocked authorization")
			}
			// A different lane completes with Retry-After while authorization is blocked.
			s.mu.Lock()
			s.finishAccountLocked(lifecycleTarget{account: binding.account, project: binding.project, lane: lifecycleMetadata}, &reviewprovider.Error{RetryAfterSeconds: 60}, clock.Now())
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
				t.Fatal("claim did not finish")
			}
			if err := got.err; err != nil {
				t.Fatal(err)
			}
			if got.target != nil {
				defer got.target.watch.entry.cancel()
			}
			if expired {
				if got.target == nil || !got.target.watch.entry.lastAttempt.Equal(clock.Now()) {
					t.Fatal("expired cooldown did not use current scheduler time", got.target)
				}
			} else {
				s.mu.Lock()
				reserved := sub.entries[0].inflight || sub.entries[0].started != 0 || !sub.entries[0].lastAttempt.IsZero() || s.lanes[lifecycleEnrichment].accounts[binding.account] || s.lanes[lifecycleEnrichment].projects[binding.project]
				s.mu.Unlock()
				if got.target != nil || reserved {
					t.Fatal("claim reserved provider work during newly recorded cooldown")
				}
				clock.Add(until.Sub(clock.Now()))
				watchTick(t, f, s)
				if reader.calls != 1 {
					t.Fatal("claim did not resume after cooldown", reader.calls)
				}
			}
		})
	}
}
