package reviewexec

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchPassedReader struct {
	*ciWatchReader
	statusID atomic.Int64
}

func (r *ciWatchPassedReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	o, err := r.ciWatchReader.ObserveCIGate(ctx, target, gate)
	if err == nil && o.Results != nil {
		o.Results.Statuses[0].ID += r.statusID.Load()
		o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
		o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	}
	return o, err
}

func TestCIWatchPassedBaselineHTTPSharedFreshQuietAndChangedProof(t *testing.T) {
	f, s, clock, base, _ := ciWatchFixture(t)
	before := lifecycleHintCounts(t, f)
	r := &ciWatchPassedReader{ciWatchReader: base}
	base.states[42] = "satisfied"
	s.readers["test-capability"] = r
	head, baseSHA := strings.Repeat("a", 40), lifecyclePull(42).BaseSHA
	type response struct {
		code  int
		value CIWatchResult
		raw   string
		ack   []string
	}
	start := func(version, key, wait string) <-chan response {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		out := make(chan response, 1)
		go func() {
			body := `{"targets":[{"pull_number":42,"head_sha":"` + head + `","base_sha":"` + baseSHA + `"`
			if key != "" {
				body += `,"after_observation_key":"` + key + `"`
			}
			body += `}]}`
			req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.reader)
			req.Header.Set("Prefer", "wait="+wait)
			req.Header.Set("X-FC-CI-Watch", version)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, req)
			var v CIWatchResult
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			out <- response{w.Code, v, w.Body.String(), append([]string(nil), w.Header().Values("X-FC-CI-Watch")...)}
		}()
		return out
	}
	receive := func(out <-chan response) response {
		t.Helper()
		select {
		case got := <-out:
			if got.code != 200 {
				t.Fatal("CI watch refused", got.code, got.raw)
			}
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("CI watch did not return")
			return response{}
		}
	}
	initial := start("change-v2", "", "10")
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	first := receive(initial).value
	if first.State != "passed" || !hash.MatchString(first.Targets[0].ObservationKey) {
		t.Fatal("initial passing proof missing", first)
	}
	key := first.Targets[0].ObservationKey
	ciWatchRegistered(t, s, 0)
	current := start("change-v3", key, "10")
	quiet := start("change-v3", key, "1")
	old1, old2 := start("change-v1", key, "10"), start("change-v2", key, "10")
	ciWatchRegistered(t, s, 4)
	// A new subscription cannot settle from the previously collected pass.
	s.mu.Lock()
	var subs []*ciWatchSubscription
	for sub := range s.watchers {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		if v := s.ciWatchResult(sub); v.State != "pending" || v.Targets[0].ObservedAt != "" || v.Targets[0].WaitingForChange {
			t.Fatal("cached passing result settled a new subscription", v)
		}
	}
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	for _, old := range []<-chan response{old1, old2} {
		got := receive(old)
		if len(got.ack) != 0 || got.value.State != "passed" || got.value.Targets[0].WaitingForChange || got.value.Targets[0].ObservationKey != key || strings.Contains(got.raw, "AllowPassedRearm") || strings.Contains(got.raw, "allow_passed") {
			t.Fatal("new caller changed old passing wake or leaked capability", got)
		}
	}
	ciWatchRegistered(t, s, 2)
	snapshot := func(state string, waiting bool) {
		t.Helper()
		select {
		case got := <-current:
			t.Fatal("unchanged or pending result woke", got)
		default:
		}
		s.mu.Lock()
		var currentSubs []*ciWatchSubscription
		for sub := range s.watchers {
			currentSubs = append(currentSubs, sub)
		}
		s.mu.Unlock()
		for _, sub := range currentSubs {
			v := s.ciWatchResult(sub)
			item := v.Targets[0]
			if v.State != "pending" || item.State != state || item.WaitingForChange != waiting || item.ObservedHeadSHA != head || item.ObservedBaseSHA != baseSHA || waiting && (item.ObservationKey != key || item.Satisfied != 1 || item.Checks[0].CommitStatus.ID != 991) {
				t.Fatal("rearm changed actual CI proof or source pins", v)
			}
		}
	}
	snapshot("passed", true)
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	snapshot("passed", true)
	expiredReply := receive(quiet)
	if !reflect.DeepEqual(expiredReply.ack, []string{"change-v3"}) {
		t.Fatal("passed rearm not explicitly acknowledged", expiredReply)
	}
	expired := expiredReply.value
	if expired.State != "pending" || !expired.TimedOut || expired.Targets[0].State != "passed" || !expired.Targets[0].WaitingForChange || expired.Targets[0].ObservationKey != key {
		t.Fatal("quiet expiry hid passing facts", expired)
	}
	ciWatchRegistered(t, s, 1)
	base.mu.Lock()
	base.states[42] = "pending"
	base.mu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	snapshot("pending", false)
	// This is a verdict baseline, not an event latch: the identical final proof
	// stays quiet even after this subscriber observed intermediate pending facts.
	base.mu.Lock()
	base.states[42] = "satisfied"
	base.mu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	snapshot("passed", true)
	// A new status identity is changed passing proof on the same head and base.
	r.statusID.Store(1)
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	changed := receive(current).value
	if changed.State != "passed" || changed.Targets[0].WaitingForChange || changed.Targets[0].ObservationKey == key || changed.Targets[0].Checks[0].CommitStatus.ID != 992 {
		t.Fatal("changed passing proof did not wake", changed)
	}
	ciWatchRegistered(t, s, 0)
	// Rearming the new pass must still surface an actual failure.
	failure := start("change-v3", changed.Targets[0].ObservationKey, "10")
	ciWatchRegistered(t, s, 1)
	base.mu.Lock()
	base.states[42] = "failed"
	base.mu.Unlock()
	watchTick(t, f, s)
	failed := receive(failure).value
	if failed.State != "failed" || failed.Targets[0].WaitingForChange || failed.Targets[0].Failed != 1 {
		t.Fatal("passing baseline concealed failure", failed)
	}
	ciWatchRegistered(t, s, 0)
	base.mu.Lock()
	calls := base.calls
	base.mu.Unlock()
	if calls != 7 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) || attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls`) != 0 {
		t.Fatal("rearm duplicated shared collections or mutated durable state", calls)
	}
}

func TestCIWatchPassedBaselineProjectionPreservesSourceAndCapabilities(t *testing.T) {
	key := strings.Repeat("d", 64)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, capable := range []bool{false, true} {
		for _, state := range []string{"passed", "failed", "pending", "closed", "superseded", "unconfigured"} {
			s := &lifecycleSchedule{}
			target := CIWatchTarget{AllowPassedRearm: capable, PullNumber: 42, HeadSHA: head, BaseSHA: base, AfterObservationKey: key}
			item := CIWatchItem{State: state, ObservationKey: key, ObservedHeadSHA: head, ObservedBaseSHA: base, Checks: []SourceCIContext{}}
			entry := &ciWatchEntry{version: 2, wanted: 2, done: 2, result: item}
			sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "one"}, targets: []CIWatchTarget{target}, minimum: []uint64{2}, bases: []string{base}, entries: []*ciWatchEntry{entry}}
			got := s.ciWatchResult(sub)
			waiting := state == "failed" || capable && state == "passed"
			want := state
			if waiting {
				want = "pending"
			}
			if got.State != want || got.Targets[0].State != state || got.Targets[0].WaitingForChange != waiting {
				t.Fatal("capability changed unrelated verdict", capable, state, got)
			}
			// A refresh hint withholds a green verdict, rather than crediting it as an
			// unchanged pass. Fresh comparison movement takes precedence over that hint.
			entry.wanted++
			if state != "closed" && state != "superseded" {
				got = s.ciWatchResult(sub)
				if got.State != "pending" || got.Targets[0].WaitingForChange {
					t.Fatal("refresh pending inherited verdict baseline", got)
				}
				for _, move := range []string{"head", "base"} {
					entry.result = item
					if move == "head" {
						entry.result.ObservedHeadSHA = strings.Repeat("e", 40)
					} else {
						entry.result.ObservedBaseSHA = strings.Repeat("e", 40)
					}
					got = s.ciWatchResult(sub)
					if got.State != "superseded" || got.Targets[0].WaitingForChange || got.Targets[0].HeadSHA != head || got.Targets[0].BaseSHA != base {
						t.Fatal("baseline or newer hint erased source movement", capable, state, move, got)
					}
				}
			}
		}
	}
}
