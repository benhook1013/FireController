package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchReader struct {
	mu         sync.Mutex
	pulls      map[int64]reviewprovider.Pull
	states     map[int64]string
	calls      int
	gates      []string
	block      chan struct{}
	entered    chan struct{}
	repoBranch string
	moveGate   bool
}

func (r *ciWatchReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branch := r.repoBranch
	if branch == "" {
		branch = "develop"
	}
	return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: branch}, nil
}
func (r *ciWatchReader) Pull(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pulls[n]
	if !ok {
		p = lifecyclePull(n)
		p.BaseRef = "stack/parent"
	}
	return p, nil
}
func (r *ciWatchReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	r.mu.Lock()
	r.calls++
	r.gates = append(r.gates, gate)
	state := r.states[target.PullNumber]
	block, entered := r.block, r.entered
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return reviewprovider.CIObservation{}, ctx.Err()
		}
	}
	if state == "" {
		state = "missing"
	}
	o := sourceCITestObservation(target, state)
	o.Authority.Branch = gate
	if state == "missing" {
		o.Results.Statuses = []reviewprovider.CICommitStatus{}
		o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
		o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	}
	r.mu.Lock()
	if r.moveGate {
		r.repoBranch = "main"
	}
	r.mu.Unlock()
	return o, nil
}
func ciWatchFixture(t *testing.T) (*attemptFixture, *lifecycleSchedule, *lifecycleTestClock, *ciWatchReader, core.Scope) {
	t.Helper()
	f := attemptProjectionFixture(t, false)
	clock := &lifecycleTestClock{now: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	options, _ := lifecycleOptions([]LifecycleSyncOptions{{Now: clock.Now}})
	s := newLifecycleSchedule(options)
	r := &ciWatchReader{pulls: map[int64]reviewprovider.Pull{}, states: map[int64]string{}}
	s.readers = map[string]TargetReader{"test-capability": r}
	attachLifecycleHints(t, f, s)
	return f, s, clock, r, operationReadScope(t, f)
}
func ciWatchRegistered(t *testing.T, s *lifecycleSchedule, n int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		s.mu.Lock()
		got := len(s.watchers)
		s.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("watch registrations did not converge", n)
}

type ciWatchReply struct {
	value CIWatchResult
	err   error
}

func startCIWatch(t *testing.T, f *attemptFixture, scope core.Scope, targets []CIWatchTarget) (<-chan ciWatchReply, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan ciWatchReply, 1)
	go func() { v, e := f.e.WatchCI(ctx, scope, targets, 300); out <- ciWatchReply{v, e} }()
	return out, cancel
}
func receiveCIWatch(t *testing.T, out <-chan ciWatchReply) ciWatchReply {
	t.Helper()
	select {
	case v := <-out:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return")
		return ciWatchReply{}
	}
}
func watchTick(t *testing.T, f *attemptFixture, s *lifecycleSchedule) *lifecycleTarget {
	t.Helper()
	target, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || target == nil || target.watch == nil {
		t.Fatal("watch did not share lifecycle enrichment claim", target, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCIWatchReadOnlyUnenrolledSharingMissingAndCancellation(t *testing.T) {
	f, s, clock, r, scope := ciWatchFixture(t)
	target := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}
	before := lifecycleHintCounts(t, f)
	one, cancelOne := startCIWatch(t, f, scope, []CIWatchTarget{target})
	defer cancelOne()
	two, cancelTwo := startCIWatch(t, f, scope, []CIWatchTarget{target})
	defer cancelTwo()
	ciWatchRegistered(t, s, 2)
	watchTick(t, f, s)
	s.mu.Lock()
	for sub := range s.watchers {
		s.mu.Unlock()
		v := s.ciWatchResult(sub)
		if v.State != "pending" || v.Targets[0].Unproven != 1 || v.Targets[0].Required != 1 || len(v.Targets[0].Checks) != 1 || v.Targets[0].GateRef != "develop" || v.Targets[0].BaseRef != "stack/parent" {
			t.Fatal("missing required check became passed or lost gate", v)
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	r.mu.Lock()
	if r.calls != 1 {
		t.Fatal("subscribers duplicated provider reads", r.calls)
	}
	r.states[42] = "satisfied"
	r.block, r.entered = make(chan struct{}), make(chan struct{}, 1)
	passBlock, passEntered := r.block, r.entered
	r.mu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	passDone := make(chan error, 1)
	go func() { passDone <- f.e.observeLifecycle(t.Context(), *claim, s) }()
	select {
	case <-passEntered:
	case <-time.After(time.Second):
		t.Fatal("passing controlled check did not start")
	}
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, HeadSHA: strings.Repeat("f", 40), CI: true})
	close(passBlock)
	if err = <-passDone; err != nil {
		t.Fatal(err)
	}
	for _, out := range []<-chan ciWatchReply{one, two} {
		select {
		case reply := <-out:
			t.Fatal("terminal result discarded known in-flight hint", reply)
		default:
		}
	}
	s.mu.Lock()
	for sub := range s.watchers {
		s.mu.Unlock()
		value := s.ciWatchResult(sub)
		if value.State != "pending" || value.Targets[0].Reason != "ci_watch_refresh_pending" || value.Targets[0].Satisfied != 1 {
			t.Fatal("known newer hint was not kept pending", value)
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	r.mu.Lock()
	r.block, r.entered = nil, nil
	r.mu.Unlock()
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	for _, out := range []<-chan ciWatchReply{one, two} {
		reply := receiveCIWatch(t, out)
		if reply.err != nil || reply.value.State != "passed" || reply.value.TimedOut || !reflect.DeepEqual(reply.value.Targets[0].CIWatchTarget, target) || reply.value.Targets[0].Satisfied != 1 || reply.value.Targets[0].Checks[0].CommitStatus == nil {
			t.Fatal("actual required check did not settle original watch", reply)
		}
	}
	ciWatchRegistered(t, s, 0)
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) || attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls`) != 0 {
		t.Fatal("read watch enrolled PR or changed review authority")
	}
	r.mu.Lock()
	if r.calls != 3 {
		t.Fatal("fallback did not share single observation", r.calls)
	}
	r.block = make(chan struct{})
	r.entered = make(chan struct{}, 1)
	entered := r.entered
	r.mu.Unlock()
	pending, cancel := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 43, HeadSHA: target.HeadSHA}})
	ciWatchRegistered(t, s, 1)
	claim, err = f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *claim, s) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("controlled collection did not start")
	}
	cancel()
	if reply := receiveCIWatch(t, pending); reply.err == nil {
		t.Fatal("cancellation became success")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("last subscriber cancellation did not detach provider read")
	}
	ciWatchRegistered(t, s, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.watches) != 0 || len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
		t.Fatal("cancellation leaked registrations or reservations")
	}
}

func TestCIWatchFreshPinsFirstTerminalAndWebhookSupersession(t *testing.T) {
	f, s, clock, r, scope := ciWatchFixture(t)
	head := strings.Repeat("a", 40)
	r.states[43] = "satisfied"
	out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head}, {PullNumber: 43, HeadSHA: head}})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	watchTick(t, f, s)
	first := receiveCIWatch(t, out)
	if first.err != nil || first.value.State != "passed" || first.value.Targets[0].State != "pending" || first.value.Targets[1].State != "passed" {
		t.Fatal("watch waited for all targets or lost actual per-target states", first)
	}
	ciWatchRegistered(t, s, 0)
	keeper, stopKeeper := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head}})
	defer stopKeeper()
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	// An explicit event for this active watch may name a test-merge commit
	// before that alias is observed. It schedules a fresh check, not success.
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, HeadSHA: strings.Repeat("f", 40), CI: true})
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	select {
	case reply := <-keeper:
		t.Fatal("unknown test-merge hint supplied CI truth", reply)
	default:
	}
	newHead := strings.Repeat("c", 40)
	next, stopNext := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: newHead}})
	defer stopNext()
	ciWatchRegistered(t, s, 2)
	select {
	case reply := <-next:
		t.Fatal("old cache superseded a new expected head", reply)
	default:
	}
	p := lifecyclePull(42)
	p.BaseRef = "stack/parent"
	p.HeadSHA = newHead
	r.mu.Lock()
	r.pulls[42] = p
	r.mu.Unlock()
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, HeadSHA: newHead, Metadata: true, CI: true})
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	old := receiveCIWatch(t, keeper)
	if old.err != nil || old.value.State != "superseded" || old.value.Targets[0].ObservedHeadSHA != newHead {
		t.Fatal("fresh source movement not reported", old)
	}
	ciWatchRegistered(t, s, 1)
	select {
	case reply := <-next:
		t.Fatal("new head inherited an old mismatch or missing check passed", reply)
	default:
	}
	// Deliver the first fresh view to the remaining subscriber before moving
	// its base. Registration alone does not prove it consumed the notification.
	s.mu.Lock()
	var remaining *ciWatchSubscription
	for sub := range s.watchers {
		remaining = sub
	}
	s.mu.Unlock()
	if v := s.ciWatchResult(remaining); v.Targets[0].ObservedBaseSHA != p.BaseSHA {
		t.Fatal("first fresh base was not pinned before movement", v)
	}
	// The first fresh base is private subscription state, not an invented request pin.
	p.BaseSHA = strings.Repeat("d", 40)
	r.mu.Lock()
	r.pulls[42] = p
	beforeCalls := r.calls
	r.mu.Unlock()
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, Metadata: true})
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	moved := receiveCIWatch(t, next)
	if moved.err != nil || moved.value.State != "superseded" || moved.value.Targets[0].BaseSHA != "" || moved.value.Targets[0].ObservedBaseSHA != p.BaseSHA {
		t.Fatal("implicit base pin was lost or echoed as caller input", moved)
	}
	r.mu.Lock()
	if r.calls != beforeCalls {
		t.Fatal("source mismatch spent full CI collection")
	}
	r.mu.Unlock()
	ciWatchRegistered(t, s, 0)
	for _, state := range []string{"not_required", "failed", "closed"} {
		r.mu.Lock()
		r.states[44] = state
		closed := lifecyclePull(44)
		closed.BaseRef = "stack/parent"
		if state == "closed" {
			closed.State = "closed"
		}
		r.pulls[44] = closed
		r.mu.Unlock()
		request := []CIWatchTarget{{PullNumber: 44, HeadSHA: head}}
		done, stop := startCIWatch(t, f, scope, request)
		ciWatchRegistered(t, s, 1)
		watchTick(t, f, s)
		reply := receiveCIWatch(t, done)
		stop()
		want := map[string]string{"not_required": "unconfigured", "failed": "failed", "closed": "closed"}[state]
		if reply.err != nil || reply.value.State != want || state == "not_required" && reply.value.Targets[0].Required != 0 {
			t.Fatal("empty/missing/failed/closed states collapsed", state, reply)
		}
		ciWatchRegistered(t, s, 0)
	}
}

func TestCIWatchHTTPReadScopeBoundsAndQuietTimeout(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/ci/watch"
	invoke := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.reader)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Prefer", "wait=0")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	body := `{"targets":[{"pull_number":42,"head_sha":"` + strings.Repeat("a", 40) + `"}]}`
	before := lifecycleHintCounts(t, f)
	w := invoke(body)
	var result CIWatchResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "pending" || !result.TimedOut || len(result.Targets) != 1 || result.Targets[0].ObservedAt != "" || result.Targets[0].Checks == nil {
		t.Fatal("read-only timeout invented facts or required write grant", w.Code, w.Body.String())
	}
	ciWatchRegistered(t, s, 0)
	for _, invalid := range []string{`{"targets":null}`, `{"targets":[]}`, strings.Replace(body, `"head_sha":`, `"base_sha":null,"head_sha":`, 1), strings.Replace(body, `}]}`, `},{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`"}]}`, 1)} {
		if w = invoke(invalid); w.Code != 400 {
			t.Fatal("invalid target input accepted", w.Code)
		}
	}
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("HTTP read watch mutated review accounting")
	}
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	reader.states[45], reader.moveGate = "satisfied", true
	reader.mu.Unlock()
	sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 45, HeadSHA: strings.Repeat("a", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	watchTick(t, f, s)
	changedGate := s.ciWatchResult(sub)
	s.unregisterCIWatch(sub)
	if changedGate.State != "pending" || changedGate.Targets[0].Reason != "ci_watch_gate_changed" {
		t.Fatal("changed default gate retained green", changedGate)
	}
	target := CIWatchItem{CIWatchTarget: CIWatchTarget{PullNumber: 1, HeadSHA: strings.Repeat("a", 40)}, State: "failed", Checks: make([]SourceCIContext, 128)}
	for i := range target.Checks {
		target.Checks[i] = SourceCIContext{Context: strings.Repeat("long", 128), State: "failed"}
	}
	value := CIWatchResult{ProjectID: attemptProject, State: "failed", Targets: make([]CIWatchItem, 20)}
	for i := range value.Targets {
		value.Targets[i] = target
	}
	if _, err := boundedCIWatchResult(value); err == nil {
		t.Fatal("unbounded context response accepted")
	}
	f.e.lifecycleRunning.Store(false)
	reader.mu.Lock()
	reader.moveGate = false
	reader.states[46] = "satisfied"
	reader.mu.Unlock()
	lifetime, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartLifecycleSync(lifetime, map[string]TargetReader{"test-capability": reader}, LifecycleSyncOptions{ScanInterval: 5 * time.Millisecond})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	active, err := f.e.WatchCI(t.Context(), scope, []CIWatchTarget{{PullNumber: 46, HeadSHA: strings.Repeat("a", 40)}}, 3)
	refreshStop(t, cancel, done)
	if err != nil || active.State != "passed" || active.Targets[0].GateRef != "main" || attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls`) != 0 {
		t.Fatal("actual lifecycle workers failed read-only unenrolled CI watch", active, err)
	}
	if w = invoke(body); w.Code != 503 {
		t.Fatal("missing scheduler accepted watch", w.Code)
	}
	var failure *core.Error
	if _, err := f.e.WatchCI(t.Context(), core.Scope{}, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}, 0); err == nil || !errors.As(err, &failure) {
		t.Fatal("unscoped watch accepted", err)
	}
}

func TestCIWatchClosedSurvivesCIHintButRechecksMetadata(t *testing.T) {
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
	s.mu.Lock()
	entry := sub.entries[0]
	entry.version = sub.minimum[0]
	entry.result = CIWatchItem{State: "closed", Reason: "ci_watch_pr_closed", ObservedHeadSHA: strings.Repeat("a", 40), Checks: []SourceCIContext{}}
	s.mu.Unlock()
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("a", 40), CI: true})
	if got := s.ciWatchResult(sub); got.State != "closed" {
		t.Fatal("CI-only hint erased freshly observed closure", got)
	}
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, Metadata: true})
	if got := s.ciWatchResult(sub); got.State != "pending" {
		t.Fatal("metadata/reopen hint failed to request fresh source", got)
	}
	closed := lifecyclePull(42)
	closed.State = "closed"
	reader.mu.Lock()
	reader.pulls[42] = closed
	reader.mu.Unlock()
	watchTick(t, f, s)
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, CI: true})
	if got := s.ciWatchResult(sub); got.State != "closed" {
		t.Fatal("completed metadata refresh kept obsolete demand", got)
	}
	closed.State = "open"
	reader.mu.Lock()
	reader.pulls[42] = closed
	reader.mu.Unlock()
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, Metadata: true})
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	if got := s.ciWatchResult(sub); got.State != "pending" || got.Targets[0].Reason == "ci_watch_refresh_pending" {
		t.Fatal("fresh reopened PR did not replace closure", got)
	}
}

func TestCIWatchNewSubscriberHeadHintInvalidatesInflightPass(t *testing.T) {
	f, s, clock, r, scope := ciWatchFixture(t)
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.unregisterCIWatch(old)
	watchTick(t, f, s)
	p := lifecyclePull(42)
	p.HeadSHA = strings.Repeat("c", 40)
	p.BaseRef = "stack/parent"
	r.mu.Lock()
	r.pulls[42] = p
	r.states[42] = "satisfied"
	r.mu.Unlock()
	sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: p.HeadSHA}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.unregisterCIWatch(sub)
	clock.Add(ciWatchFallbackInterval)
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, HeadSHA: p.HeadSHA, CI: true})
	if err := f.e.observeLifecycle(t.Context(), *claim, s); err != nil {
		t.Fatal(err)
	}
	if got := s.ciWatchResult(sub); got.State != "pending" || got.Targets[0].Reason != "ci_watch_refresh_pending" {
		t.Fatal("pre-hint pass escaped the newer active head", got)
	}
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	if got := s.ciWatchResult(sub); got.State != "passed" {
		t.Fatal("fresh post-hint proof did not settle", got)
	}
}

func TestCIWatchCannotOutliveItsCapturedSchedule(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			f, s, _, _, scope := ciWatchFixture(t)
			out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
			defer cancel()
			ciWatchRegistered(t, s, 1)
			// Model the completed stop/restart before the old waiter consumes its
			// shutdown notification. The global running flag already belongs to new.
			replacement := newLifecycleSchedule(s.options)
			replacement.readers = s.readers
			f.e.lifecycleMu.Lock()
			f.e.lifecycleSchedule = replacement
			f.e.lifecycleRunning.Store(true)
			f.e.lifecycleMu.Unlock()
			s.mu.Lock()
			for sub := range s.watchers {
				if terminal {
					entry := sub.entries[0]
					entry.version = sub.minimum[0]
					entry.result = CIWatchItem{State: "closed", Reason: "ci_watch_pr_closed", Checks: []SourceCIContext{}}
				}
				select {
				case sub.changed <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
			reply := receiveCIWatch(t, out)
			var failure *core.Error
			if !errors.As(reply.err, &failure) || failure.Code != "overloaded" {
				t.Fatal("old scheduler returned facts or remained attached after restart", reply)
			}
			ciWatchRegistered(t, s, 0)
		})
	}
}

type ciWatchRegistrationBarrierReader struct {
	*ciWatchReader
	entered, release chan struct{}
}

func (r *ciWatchRegistrationBarrierReader) SupportsCIGate() bool {
	close(r.entered)
	<-r.release
	return true
}

func TestCIWatchRegistrationSharesScheduleLifetimeFence(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	barrier := &ciWatchRegistrationBarrierReader{ciWatchReader: reader, entered: make(chan struct{}), release: make(chan struct{})}
	s.readers["test-capability"] = barrier
	out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
	defer cancel()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("registration did not enter")
	}
	// Registration is known to be inside SupportsCIGate, so checking the
	// lifetime lock here cannot pass merely because restart was not scheduled.
	if f.e.lifecycleMu.TryLock() {
		f.e.lifecycleMu.Unlock()
		close(barrier.release)
		cancel()
		receiveCIWatch(t, out)
		t.Fatal("registration did not hold the schedule lifetime read lock")
	}
	restarted := make(chan struct{})
	go func() {
		f.e.lifecycleMu.Lock()
		replacement := newLifecycleSchedule(s.options)
		replacement.readers = s.readers
		f.e.lifecycleSchedule = replacement
		f.e.lifecycleRunning.Store(true)
		f.e.lifecycleMu.Unlock()
		close(restarted)
	}()
	close(barrier.release)
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart remained blocked after registration")
	}
	s.mu.Lock()
	for sub := range s.watchers {
		select {
		case sub.changed <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	reply := receiveCIWatch(t, out)
	var failure *core.Error
	if !errors.As(reply.err, &failure) || failure.Code != "overloaded" {
		t.Fatal("registered old schedule survived replacement", reply)
	}
	ciWatchRegistered(t, s, 0)
}

func TestCIWatchSubscriberFreshnessDoesNotWithholdOtherOwners(t *testing.T) {
	for _, pass := range []bool{false, true} {
		name := "failed"
		if pass {
			name = "passed"
		}
		t.Run(name, func(t *testing.T) {
			f, s, clock, base, scope := ciWatchFixture(t)
			r := &ciWatchRerunReader{ciWatchReader: base, attempt: 1, pass: pass}
			s.readers["test-capability"] = r
			binding, err := f.e.ciWatchBinding(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			targets := []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}
			register := func() *ciWatchSubscription {
				t.Helper()
				sub, err := s.registerCIWatch(scope, binding, targets)
				if err != nil {
					t.Fatal(err)
				}
				return sub
			}
			first, shared := register(), register()
			defer s.unregisterCIWatch(first)
			defer s.unregisterCIWatch(shared)
			before := lifecycleHintCounts(t, f)
			watchTick(t, f, s)
			original := s.ciWatchResult(first)
			if original.State != name || s.ciWatchResult(shared).State != name || r.calls != 1 {
				t.Fatal("pre-claim owners did not share one fresh terminal collection", original, r.calls)
			}
			late := register()
			defer s.unregisterCIWatch(late)
			if got := s.ciWatchResult(first); !reflect.DeepEqual(got, original) {
				t.Fatal("new subscriber withheld another owners fresh terminal result", got, original)
			}
			if got := s.ciWatchResult(late); got.State != "pending" || got.Targets[0].ObservedAt != "" {
				t.Fatal("new owner reused a cached terminal result", got)
			}
			if claim, err := f.e.claimCIWatch(t.Context(), s.readers, s); err != nil || claim != nil {
				t.Fatal("subscriber freshness bypassed event coalescing", claim, err)
			}
			clock.Add(5 * time.Second)
			watchTick(t, f, s)
			if got := s.ciWatchResult(late); got.State != name || r.calls != 2 {
				t.Fatal("new owner did not get its own fresh shared collection", got, r.calls)
			}
			// A zero-wait caller leaves immediately; it must not leave a shared
			// event hint or provider work behind while other owners remain.
			for i := 0; i < 3; i++ {
				gone := register()
				s.unregisterCIWatch(gone)
				if got := s.ciWatchResult(first); got.State != name {
					t.Fatal("departed subscriber invalidated another owner", got)
				}
			}
			clock.Add(5 * time.Second)
			if claim, err := f.e.claimCIWatch(t.Context(), s.readers, s); err != nil || claim != nil || r.calls != 2 {
				t.Fatal("departed subscriber forced an orphaned provider collection", claim, err, r.calls)
			}
			// Real CI hints still invalidate shared proof and request a collection.
			s.hintCIWatches(githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, CI: true})
			if got := s.ciWatchResult(first); got.State != "pending" || got.Targets[0].Reason != "ci_watch_refresh_pending" {
				t.Fatal("real CI hint did not invalidate older terminal facts", got)
			}
			watchTick(t, f, s)
			if got := s.ciWatchResult(first); got.State != name || r.calls != 3 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
				t.Fatal("real hint lost shared observation or changed durable state", got, r.calls)
			}
		})
	}
}
