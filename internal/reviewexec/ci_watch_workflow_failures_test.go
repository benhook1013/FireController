package reviewexec

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchWorkflowReader struct {
	*ciWatchReader
	workflowMu  sync.Mutex
	conclusions map[int64]string
	selections  [][]int64
	warning     error
}

func (r *ciWatchWorkflowReader) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, gate string, ids []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	r.workflowMu.Lock()
	r.selections = append(r.selections, append([]int64(nil), ids...))
	r.workflowMu.Unlock()
	o, err := r.ciWatchReader.ObserveCIGate(ctx, target, gate)
	if err != nil {
		return o, nil, err
	}
	rows := []reviewprovider.CIWorkflowFailure{}
	r.workflowMu.Lock()
	defer r.workflowMu.Unlock()
	for _, id := range ids {
		o.Actions.Runs = append(o.Actions.Runs, reviewprovider.ActionsHeadRun{ID: id * 10, WorkflowID: id, CheckSuiteID: id * 100, RunNumber: 1, RunAttempt: 2, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: o.SelectedCommitSHA, Event: "pull_request", Status: "in_progress", CreatedAt: o.ObservedAt.Add(-time.Minute), UpdatedAt: o.ObservedAt})
		if conclusion := r.conclusions[id]; conclusion != "" {
			rows = append(rows, reviewprovider.CIWorkflowFailure{WorkflowID: id, RunID: id * 10, RunAttempt: 2, JobID: id * 1000, CheckRunID: id * 10000, Name: "Explicit leaf", Conclusion: conclusion})
		}
	}
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o, rows, r.warning
}

func TestCIWatchWorkflowFailuresShareFilterAndRetainGateTruth(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{10: "failure", 20: "failure", 30: "failure"}}
	s.readers["test-capability"] = r
	before := lifecycleHintCounts(t, f)
	head := strings.Repeat("a", 40)
	plain, stopPlain := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head}})
	defer stopPlain()
	a, stopA := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}}})
	defer stopA()
	b, stopB := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{20}}})
	defer stopB()
	ciWatchRegistered(t, s, 3)
	watchTick(t, f, s)
	first := receiveCIWatch(t, a)
	second := receiveCIWatch(t, b)
	for i, reply := range []ciWatchReply{first, second} {
		id := int64(10 + i*10)
		if reply.err != nil || reply.value.State != "attention" || reply.value.TimedOut || reply.value.Targets[0].State != "pending" || reply.value.Targets[0].Pending != 1 || reply.value.Targets[0].Failed != 0 || len(reply.value.Targets[0].WorkflowFailures) != 1 || reply.value.Targets[0].WorkflowFailures[0].WorkflowID != id {
			t.Fatal("attention changed gate or leaked another owner's selection", reply)
		}
	}
	select {
	case value := <-plain:
		t.Fatal("gate-only owner received optional attention", value)
	default:
	}
	key := first.value.Targets[0].ObservationKey
	baseline, stopBaseline := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}, AfterObservationKey: key}})
	defer stopBaseline()
	ciWatchRegistered(t, s, 2)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	select {
	case value := <-baseline:
		t.Fatal("unchanged attention woke a baseline", value)
	default:
	}
	s.mu.Lock()
	for sub := range s.watchers {
		if len(sub.targets[0].FailureWorkflowIDs) > 0 {
			s.mu.Unlock()
			v := s.ciWatchResult(sub)
			if !v.Targets[0].WaitingForChange || v.Targets[0].State != "pending" || len(v.Targets[0].WorkflowFailures) != 1 || v.Targets[0].ObservationKey != key {
				t.Fatal("another owner's withdrawal changed baseline", v)
			}
			s.mu.Lock()
		}
	}
	s.mu.Unlock()
	// A new selection arriving during a collection requires the next claimed
	// generation. Its IDs cannot be added retroactively to the in-flight read.
	r.mu.Lock()
	r.block = make(chan struct{})
	r.entered = make(chan struct{}, 1)
	block, entered := r.block, r.entered
	r.mu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *claim, s) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("collection did not start")
	}
	late, stopLate := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{30}}})
	defer stopLate()
	ciWatchRegistered(t, s, 3)
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-late:
		t.Fatal("new owner used an older claim", value)
	default:
	}
	r.mu.Lock()
	r.block, r.entered = nil, nil
	r.mu.Unlock()
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	if reply := receiveCIWatch(t, late); reply.err != nil || reply.value.State != "attention" || len(reply.value.Targets[0].WorkflowFailures) != 1 || reply.value.Targets[0].WorkflowFailures[0].WorkflowID != 30 {
		t.Fatal("new owner missed its fresh selection", reply)
	}
	r.workflowMu.Lock()
	r.conclusions[10] = "timed_out"
	r.workflowMu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	changed := receiveCIWatch(t, baseline)
	if changed.err != nil || changed.value.State != "attention" || changed.value.Targets[0].ObservationKey == key || changed.value.Targets[0].WorkflowFailures[0].Conclusion != "timed_out" {
		t.Fatal("new selected failure did not wake", changed)
	}
	stopPlain()
	if reply := receiveCIWatch(t, plain); reply.err == nil {
		t.Fatal("cancel became gate completion")
	}
	ciWatchRegistered(t, s, 0)
	r.workflowMu.Lock()
	selections := r.selections
	r.workflowMu.Unlock()
	if !reflect.DeepEqual(selections, [][]int64{{10, 20}, {10}, {10}, {10, 30}, {10}}) {
		t.Fatal("claims did not snapshot bounded owner union", selections)
	}
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("attention changed durable state")
	}
	// The union limit is enforced atomically before adding an owner.
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 16)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	retained, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: ids}})
	if err != nil {
		t.Fatal(err)
	}
	ids[0] = 999
	if retained.targets[0].FailureWorkflowIDs[0] != 1 {
		t.Fatal("request slice was not owned")
	}
	_, err = s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{17}}})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "ci_watch_workflow_capacity" {
		t.Fatal("shared overflow admitted", err)
	}
	ciWatchRegistered(t, s, 1)
	s.unregisterCIWatch(retained)
	s.readers["test-capability"] = base
	_, err = s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}}})
	if !errors.As(err, &failure) || failure.Code != "ci_watch_workflow_failures_unsupported" {
		t.Fatal("unsupported adapter silently ignored selection", err)
	}
	ciWatchRegistered(t, s, 0)
}

func TestCIWatchWorkflowFailuresStrictInputAndProjection(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	r := &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{}}
	s.readers["test-capability"] = r
	head := strings.Repeat("a", 40)
	call := func(raw, header string) *httptest.ResponseRecorder {
		body := `{"targets":[{"pull_number":42,"head_sha":"` + head + `","failure_workflow_ids":` + raw + `}]}`
		req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set("Prefer", "wait=0")
		if header != "" {
			req.Header.Set("X-FC-CI-Watch", header)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		return w
	}
	for _, raw := range []string{`null`, `[]`, `[0]`, `[-1]`, `[1,1]`, `["1"]`, `[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]`} {
		if w := call(raw, "change-v1"); w.Code != 400 {
			t.Fatal("invalid selection accepted", raw, w.Code)
		}
	}
	if w := call(`[10]`, ""); w.Code != 400 {
		t.Fatal("old wire accepted new selection", w.Code)
	}
	if w := call(`[10]`, "change-v1"); w.Code != 200 || strings.Contains(w.Body.String(), "workflow_failures") {
		t.Fatal("valid immediate read manufactured attention", w.Code, w.Body.String())
	}
	if base.calls != 0 {
		t.Fatal("input/read path started a provider poll", base.calls)
	}
	key := strings.Repeat("d", 64)
	row := reviewprovider.CIWorkflowFailure{WorkflowID: 10, RunID: 100, RunAttempt: 2, JobID: 1000, CheckRunID: 10000, Name: "Failed leaf", Conclusion: "failure"}
	for _, test := range []struct {
		name, state           string
		fresh, refresh, moved bool
		want                  string
		attention             bool
	}{
		{"attention", "pending", true, false, false, "attention", true},
		{"refreshing", "pending", true, true, false, "pending", false},
		{"old", "pending", false, false, false, "pending", false},
		{"moved", "pending", true, false, true, "superseded", false},
		{"passed", "passed", true, false, false, "passed", false},
		{"failed", "failed", true, false, false, "failed", false},
		{"closed", "closed", true, false, false, "closed", false},
		{"unconfigured", "unconfigured", true, false, false, "unconfigured", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			version := uint64(1)
			if test.fresh {
				version = 2
			}
			wanted := uint64(1)
			if test.refresh {
				wanted = 2
			}
			observed := head
			if test.moved {
				observed = strings.Repeat("b", 40)
			}
			entry := &ciWatchEntry{version: version, wanted: wanted, done: 1, result: CIWatchItem{State: test.state, ObservedHeadSHA: observed, ObservedBaseSHA: strings.Repeat("c", 40), CICommitSHA: head, Selection: "head", ObservationKey: key, WorkflowFailures: []reviewprovider.CIWorkflowFailure{row}, Checks: []SourceCIContext{}}}
			sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "one"}, targets: []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}}}, minimum: []uint64{2}, bases: []string{""}, entries: []*ciWatchEntry{entry}}
			value := s.ciWatchResult(sub)
			if value.State != test.want || (len(value.Targets[0].WorkflowFailures) > 0) != test.attention {
				t.Fatal("attention crossed freshness/terminal boundary", value)
			}
			if test.attention {
				copy := value.Targets[0]
				sub.targets[0].AfterObservationKey = copy.ObservationKey
				quiet := s.ciWatchResult(sub)
				if quiet.State != "pending" || !quiet.Targets[0].WaitingForChange || quiet.Targets[0].State != "pending" {
					t.Fatal("known attention not suppressed", quiet)
				}
			}
		})
	}
	// Selection order is not semantic; changing the selected scope is.
	if ciWatchWorkflowKey(key, []int64{10, 20}, []reviewprovider.CIWorkflowFailure{row}) != ciWatchWorkflowKey(key, []int64{20, 10}, []reviewprovider.CIWorkflowFailure{row}) || ciWatchWorkflowKey(key, []int64{10}, []reviewprovider.CIWorkflowFailure{row}) == ciWatchWorkflowKey(key, []int64{20}, []reviewprovider.CIWorkflowFailure{row}) {
		t.Fatal("selection key ignored scope or depended on ordering")
	}
}

func TestCIWatchWorkflowFailuresRejectUnknownOrMalformedIdentity(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	o := sourceCITestObservation(target, "pending")
	o.Actions.Runs = []reviewprovider.ActionsHeadRun{{ID: 100, WorkflowID: 10, CheckSuiteID: 1000, RunNumber: 1, RunAttempt: 2, RepositoryID: 801, Repository: target.Repository, HeadSHA: target.HeadSHA, Event: "pull_request", Status: "in_progress", CreatedAt: o.ObservedAt.Add(-time.Minute), UpdatedAt: o.ObservedAt}}
	row := reviewprovider.CIWorkflowFailure{WorkflowID: 10, RunID: 100, RunAttempt: 2, JobID: 1000, CheckRunID: 10000, Name: "Failed leaf", Conclusion: "failure"}
	if len(ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{row})) != 1 {
		t.Fatal("valid selected current identity refused")
	}
	for name, mutate := range map[string]func(*reviewprovider.CIWorkflowFailure){
		"foreign workflow": func(r *reviewprovider.CIWorkflowFailure) { r.WorkflowID = 20 },
		"old run":          func(r *reviewprovider.CIWorkflowFailure) { r.RunID++ },
		"old attempt":      func(r *reviewprovider.CIWorkflowFailure) { r.RunAttempt = 1 },
		"missing job":      func(r *reviewprovider.CIWorkflowFailure) { r.JobID = 0 },
		"missing check":    func(r *reviewprovider.CIWorkflowFailure) { r.CheckRunID = 0 },
		"unsafe name":      func(r *reviewprovider.CIWorkflowFailure) { r.Name = "private\x00body" },
		"cancelled":        func(r *reviewprovider.CIWorkflowFailure) { r.Conclusion = "cancelled" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := row
			mutate(&copy)
			if len(ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{copy})) != 0 {
				t.Fatal("unproven optional failure emitted", copy)
			}
		})
	}
	if len(ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{row, row})) != 0 {
		t.Fatal("duplicate workflow emitted")
	}
	o.Candidate.State = "unproven"
	if len(ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{row})) != 0 {
		t.Fatal("unknown comparison emitted attention")
	}
}

func TestCIWatchWorkflowWarningPreservesGateAndAccountBackoff(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{10: "failure"}, warning: &reviewprovider.CIWorkflowAttentionError{Cause: &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120, Status: 429, Message: "PRIVATE_PROVIDER_BODY"}}}
	s.readers["test-capability"] = r
	pin := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}
	out, stop := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer stop()
	ciWatchRegistered(t, s, 1)
	claimed := watchTick(t, f, s)
	reply := receiveCIWatch(t, out)
	if reply.err != nil || reply.value.State != "attention" || reply.value.Targets[0].State != "pending" || reply.value.Targets[0].Pending != 1 || len(reply.value.Targets[0].WorkflowFailures) != 1 {
		t.Fatal("optional warning erased gate/prior proven attention", reply)
	}
	s.mu.Lock()
	account := s.accounts[claimed.account]
	s.mu.Unlock()
	if account.lastErrorCode != "provider_rate_limited" || account.until != clock.Now().Add(120*time.Second) {
		t.Fatal("optional provider limit lost account backoff", account)
	}
	base.mu.Lock()
	base.states[42] = "satisfied"
	base.mu.Unlock()
	passed, cancel := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancel()
	ciWatchRegistered(t, s, 1)
	next, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || next != nil {
		t.Fatal("rate-limited optional path bypassed shared capacity", next, err)
	}
	clock.Add(120 * time.Second)
	watchTick(t, f, s)
	green := receiveCIWatch(t, passed)
	if green.err != nil || green.value.State != "passed" || green.value.Targets[0].Satisfied != 1 || len(green.value.Targets[0].WorkflowFailures) != 0 {
		t.Fatal("optional warning invalidated proved pass", green)
	}
}

type ciWatchFinalRepositoryFailureReader struct {
	*ciWatchWorkflowReader
	repositoryMu sync.Mutex
	reads        int
	finalError   error
	blocked      chan struct{}
}

func (r *ciWatchFinalRepositoryFailureReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.repositoryMu.Lock()
	r.reads++
	final := r.reads%2 == 0
	err, blocked := r.finalError, r.blocked
	r.repositoryMu.Unlock()
	if final {
		if blocked != nil {
			close(blocked)
			<-ctx.Done()
			return reviewprovider.Repository{}, ctx.Err()
		}
		return reviewprovider.Repository{}, err
	}
	return r.ciWatchReader.Repository(ctx, id)
}

func TestCIWatchWorkflowWarningSurvivesFinalRepositoryFailureAndCancellation(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchFinalRepositoryFailureReader{ciWatchWorkflowReader: &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{10: "failure"}, warning: &reviewprovider.CIWorkflowAttentionError{Cause: &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120, Status: 429, Message: "PRIVATE_OPTIONAL_BODY"}}}}
	s.readers["test-capability"] = r
	pin := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}
	for _, test := range []struct {
		name  string
		err   error
		delay time.Duration
	}{
		{"generic final read", &reviewprovider.Error{Code: "provider_unavailable", Message: "PRIVATE_FINAL_BODY"}, 120 * time.Second},
		{"stronger final limit", &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 240, Status: 429}, 240 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			r.repositoryMu.Lock()
			r.finalError = test.err
			r.repositoryMu.Unlock()
			out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{pin})
			defer cancel()
			ciWatchRegistered(t, s, 1)
			claim := watchTick(t, f, s)
			select {
			case value := <-out:
				t.Fatal("failed final identity check became actionable", value)
			default:
			}
			s.mu.Lock()
			account := s.accounts[claim.account]
			var sub *ciWatchSubscription
			for value := range s.watchers {
				sub = value
			}
			s.mu.Unlock()
			value := s.ciWatchResult(sub)
			if value.State != "pending" || value.Targets[0].State != "pending" || len(value.Targets[0].WorkflowFailures) != 0 || value.Targets[0].ObservationKey != "" || value.Targets[0].Reason != sourceCIErrorReason(t.Context(), test.err) {
				t.Fatal("final repository failure lost its unproven guard", value)
			}
			if account.lastErrorCode != "provider_rate_limited" || account.until != clock.Now().Add(test.delay) {
				t.Fatal("strongest observed limit was discarded", account, test.delay)
			}
			cancel()
			if value := receiveCIWatch(t, out); value.err == nil {
				t.Fatal("cancel became completion")
			}
			ciWatchRegistered(t, s, 0)
			clock.Add(test.delay)
		})
	}
	// Last-owner cancellation interrupts the final read after the optional
	// provider has already supplied a positive account Retry-After.
	r.repositoryMu.Lock()
	r.blocked = make(chan struct{})
	blocked := r.blocked
	r.repositoryMu.Unlock()
	out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	ciWatchRegistered(t, s, 1)
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *claim, s) }()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("final read never started")
	}
	cancel()
	if value := receiveCIWatch(t, out); value.err == nil {
		t.Fatal("interruption became gate result")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled final read did not join")
	}
	ciWatchRegistered(t, s, 0)
	s.mu.Lock()
	account := s.accounts[claim.account]
	s.mu.Unlock()
	if account.lastErrorCode != "provider_rate_limited" || account.until.Before(clock.Now().Add(120*time.Second)) {
		t.Fatal("caller cancellation discarded proven provider limit", account)
	}
	fresh, stop := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer stop()
	ciWatchRegistered(t, s, 1)
	if next, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment); err != nil || next != nil {
		t.Fatal("fresh subscriber bypassed retained account limit", next, err)
	}
	stop()
	receiveCIWatch(t, fresh)
	ciWatchRegistered(t, s, 0)
}
