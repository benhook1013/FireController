package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"
)

func sharedLifecycleFixture(t *testing.T) (*attemptFixture, *lifecycleSchedule, *ciWatchReader, *lifecycleTarget) {
	t.Helper()
	f, s, _ := lifecycleFixture(t)
	r := &ciWatchReader{pulls: map[int64]reviewprovider.Pull{2: lifecyclePull(2)}, states: map[int64]string{2: "satisfied"}}
	s.readers = map[string]TargetReader{"test-capability": r}
	attachLifecycleHints(t, f, s)
	m, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleMetadata)
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *m, s); err != nil {
		t.Fatal(err)
	}
	target, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || target == nil || target.watch != nil {
		t.Fatal(target, err)
	}
	return f, s, r, target
}
func TestLifecycleSharesFreshWholeGateWithWaitingWatch(t *testing.T) {
	f, s, r, target := sharedLifecycleFixture(t)
	out, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	reply := receiveCIWatch(t, out)
	if reply.err != nil || reply.value.State != "passed" {
		t.Fatal(reply)
	}
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 1 {
		t.Fatal("whole gate duplicated", calls)
	}
	if lifecycleRead(t, f).CIState != "success" {
		t.Fatal("independent lifecycle publication lost")
	}
}
func TestLifecycleSharingDoesNotSatisfyLateSubscriber(t *testing.T) {
	f, s, r, target := sharedLifecycleFixture(t)
	r.block = make(chan struct{})
	r.entered = make(chan struct{}, 1)
	first, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(context.Background(), *target, s) }()
	<-r.entered
	late, cancelLate := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	defer cancelLate()
	ciWatchRegistered(t, s, 2)
	close(r.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reply := receiveCIWatch(t, first); reply.err != nil || reply.value.State != "passed" {
		t.Fatal(reply)
	}
	select {
	case reply := <-late:
		t.Fatal("late subscriber got pre-start facts", reply)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLifecycleSharingRefusesChangedPublicationBinding(t *testing.T) {
	for _, change := range []string{"configuration", "fingerprint", "source"} {
		t.Run(change, func(t *testing.T) {
			f, s, r, target := sharedLifecycleFixture(t)
			r.block = make(chan struct{})
			r.entered = make(chan struct{}, 1)
			out, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
			defer cancel()
			ciWatchRegistered(t, s, 1)
			done := make(chan error, 1)
			go func() { done <- f.e.observeLifecycle(t.Context(), *target, s) }()
			<-r.entered
			if change == "configuration" {
				revision := int64(2)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "sharing-config-move", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2}, Channels: []string{"cli", "hosted"}, Paused: true})
				if err != nil {
					close(r.block)
					<-done
					t.Fatal(err)
				}
			} else if change == "source" {
				r.mu.Lock()
				moved := r.pulls[2]
				moved.HeadSHA = strings.Repeat("c", 40)
				r.pulls[2] = moved
				r.mu.Unlock()
				s.mu.Lock()
				key := lifecycleKey{target.project, target.number}
				due := s.due[key]
				due.next = time.Time{}
				s.due[key] = due
				s.mu.Unlock()
				metadata, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleMetadata)
				if err != nil || metadata == nil {
					close(r.block)
					<-done
					t.Fatal(metadata, err)
				}
				if err := f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
					close(r.block)
					<-done
					t.Fatal(err)
				}
			} else {
				cap := f.e.capabilities["test-capability"]
				cap.Fingerprint = "changed-fingerprint"
				f.e.capabilities["test-capability"] = cap
			}
			close(r.block)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case reply := <-out:
				if reply.value.State == "passed" {
					t.Fatal("changed binding published stale authority", reply)
				}
			case <-time.After(20 * time.Millisecond):
			}
			s.mu.Lock()
			ci := s.due[lifecycleKey{target.project, target.number}].ci
			s.mu.Unlock()
			if !ci.IsZero() {
				t.Fatal("changed binding credited lifecycle CI", ci)
			}
		})
	}
}
func TestLifecycleSharingSubscriberCancelDoesNotCancelCollection(t *testing.T) {
	f, s, r, target := sharedLifecycleFixture(t)
	r.block = make(chan struct{})
	r.entered = make(chan struct{}, 1)
	_, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	ciWatchRegistered(t, s, 1)
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *target, s) }()
	<-r.entered
	cancel()
	ciWatchRegistered(t, s, 0)
	close(r.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lifecycleRead(t, f).CIState != "success" {
		t.Fatal("watch canceled independent lifecycle")
	}
}

func TestLifecycleSharingRetainsHintDuringCollection(t *testing.T) {
	f, s, r, target := sharedLifecycleFixture(t)
	r.block = make(chan struct{})
	r.entered = make(chan struct{}, 1)
	out, cancel := startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *target, s) }()
	<-r.entered
	s.mu.Lock()
	for _, entry := range s.watches {
		entry.wanted++
	}
	s.mu.Unlock()
	close(r.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-out:
		t.Fatal("inflight hint consumed by old shared proof", reply)
	case <-time.After(20 * time.Millisecond):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.watches {
		if entry.done == entry.wanted {
			t.Fatal("hint generation lost")
		}
	}
}

type sharingMismatchReader struct {
	*ciWatchReader
	last reviewprovider.CIObservation
}

func (r *sharingMismatchReader) ObserveCIGate(_ context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	o := sourceCINonstrictMismatchObservation(target)
	authority := sourceCITestObservation(target, "satisfied").Authority
	authority.Branch = gate
	o.Authority = authority
	r.last = o
	return o, nil
}
func TestLifecycleSharingProjectionMatchesDirectMismatch(t *testing.T) {
	f, s, _, base, scope := ciWatchFixture(t)
	r := &sharingMismatchReader{ciWatchReader: base}
	s.readers["test-capability"] = r
	_, cancel := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	claim := watchTick(t, f, s)
	s.mu.Lock()
	direct := claim.watch.entry.result
	s.mu.Unlock()
	shared, ok := lifecycleCIWatchItem(attemptProject, r.last)
	if !ok || direct.MergeBaseMismatch == nil || shared.MergeBaseMismatch == nil || *direct.MergeBaseMismatch != *shared.MergeBaseMismatch || direct.ObservationKey != shared.ObservationKey {
		t.Fatal("shared mismatch differs from ordinary watch", direct, shared, ok)
	}
	before := *shared.MergeBaseMismatch
	r.last.Candidate.MergeBaseMismatch.LiveBaseSHA = strings.Repeat("f", 40)
	if *shared.MergeBaseMismatch != before {
		t.Fatal("shared mismatch aliases provider facts")
	}
}
