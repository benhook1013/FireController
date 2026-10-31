package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
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
		if reply.err != nil || reply.value.State != "passed" || reply.value.TimedOut || reply.value.Targets[0].CIWatchTarget != target || reply.value.Targets[0].Satisfied != 1 || reply.value.Targets[0].Checks[0].CommitStatus == nil {
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
