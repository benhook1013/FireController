package reviewexec

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchRerunReader struct {
	*ciWatchReader
	changeMu sync.Mutex
	attempt  int64
	pass     bool
}

func (r *ciWatchRerunReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	if _, err := r.ciWatchReader.ObserveCIGate(ctx, target, gate); err != nil {
		return reviewprovider.CIObservation{}, err
	}
	r.changeMu.Lock()
	attempt, pass := r.attempt, r.pass
	r.changeMu.Unlock()
	o := sourceCIAppStatusObservation(target, "success", false)
	o.Authority.Branch = gate
	conclusion := "failure"
	if pass {
		conclusion = "success"
	}
	o.Results.Checks[0].Conclusion = conclusion
	o.Actions.Runs[0].RunAttempt, o.Actions.Runs[0].Conclusion = attempt, conclusion
	o.Attempts[0].RunAttempt, o.Attempts[0].Conclusion = attempt, conclusion
	o.Attempts[0].Jobs[0].Conclusion = conclusion
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o, nil
}

func TestCIWatchSameFailedObservationWaitsWithoutChangingGate(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	r := &ciWatchRerunReader{ciWatchReader: base, attempt: 1}
	s.readers["test-capability"] = r
	head := strings.Repeat("a", 40)
	path := "/api/v1/projects/" + attemptProject + "/reviews/ci/watch"
	before := lifecycleHintCounts(t, f)
	request := func(after, header, wait string) *httptest.ResponseRecorder {
		body := `{"targets":[{"pull_number":42,"head_sha":"` + head + `"`
		if after != "" {
			body += `,"base_sha":"` + lifecyclePull(42).BaseSHA + `","after_observation_key":` + after
		}
		body += `}]}`
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set("Prefer", "wait="+wait)
		if header != "" {
			req.Header.Set("X-FC-CI-Watch", header)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		return w
	}
	async := func(after, header, wait string) <-chan *httptest.ResponseRecorder {
		out := make(chan *httptest.ResponseRecorder, 1)
		go func() { out <- request(after, header, wait) }()
		return out
	}
	read := func(out <-chan *httptest.ResponseRecorder) (CIWatchResult, string) {
		t.Helper()
		select {
		case w := <-out:
			var v CIWatchResult
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			return v, w.Body.String()
		case <-time.After(2 * time.Second):
			t.Fatal("HTTP CI watch did not return")
			return CIWatchResult{}, ""
		}
	}
	initial := async("", "change-v1", "2")
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	first, _ := read(initial)
	if first.State != "failed" || first.TimedOut || len(first.Targets) != 1 || !hash.MatchString(first.Targets[0].ObservationKey) || first.Targets[0].Checks[0].CheckRun.ID != 300 {
		t.Fatal("initial actual failure lost", first)
	}
	key := first.Targets[0].ObservationKey
	ciWatchRegistered(t, s, 0)
	pin := CIWatchTarget{PullNumber: 42, HeadSHA: head, BaseSHA: first.Targets[0].ObservedBaseSHA, AfterObservationKey: key}
	watch, cancel := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancel()
	legacy := async("", "", "2")
	ciWatchRegistered(t, s, 2)
	watchTick(t, f, s)
	old, oldBody := read(legacy)
	if old.State != "failed" || old.Targets[0].ObservationKey != "" || strings.Contains(oldBody, "observation_key") || strings.Contains(oldBody, "waiting_for_change") {
		t.Fatal("legacy response changed", oldBody)
	}
	ciWatchRegistered(t, s, 1)
	assertWaiting := func() {
		t.Helper()
		select {
		case reply := <-watch:
			t.Fatal("same failure ended baseline wait", reply)
		default:
		}
		s.mu.Lock()
		var sub *ciWatchSubscription
		for v := range s.watchers {
			sub = v
		}
		s.mu.Unlock()
		v := s.ciWatchResult(sub)
		item := v.Targets[0]
		if v.State != "pending" || item.State != "failed" || !item.WaitingForChange || item.ObservationKey != key || item.Failed != 1 || item.Checks[0].CheckRun.ID != 300 || item.Checks[0].CheckRun.Conclusion != "failure" {
			t.Fatal("baseline concealed gate failure", v)
		}
	}
	assertWaiting()
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	assertWaiting()
	// A new caller's quiet timeout must retain the actual failed gate and key.
	quiet := async(`"`+key+`"`, "change-v1", "1")
	ciWatchRegistered(t, s, 2)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	timed, _ := read(quiet)
	if !timed.TimedOut || timed.State != "pending" || timed.Targets[0].State != "failed" || !timed.Targets[0].WaitingForChange || timed.Targets[0].ObservationKey != key {
		t.Fatal("quiet expiry changed failed facts", timed)
	}
	ciWatchRegistered(t, s, 1)
	for _, tc := range []struct{ after, header string }{{`"` + key + `"`, ""}, {`null`, "change-v1"}, {`""`, "change-v1"}, {`"bad"`, "change-v1"}, {`"` + key + `"`, "other"}} {
		if w := request(tc.after, tc.header, "0"); w.Code != 400 {
			t.Fatal("unsupported baseline accepted", tc, w.Code)
		}
	}
	// The same run/check ID with a newly proven numbered attempt is new proof,
	// even when that attempt fails too. It must wake without granting success.
	r.changeMu.Lock()
	r.attempt = 2
	r.changeMu.Unlock()
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	changed := receiveCIWatch(t, watch)
	if changed.err != nil || changed.value.State != "failed" || changed.value.Targets[0].WaitingForChange || changed.value.Targets[0].ObservationKey == key || changed.value.Targets[0].Checks[0].CheckRun.ID != 300 {
		t.Fatal("new failed attempt did not wake", changed)
	}
	ciWatchRegistered(t, s, 0)
	// An actual success always wakes; baseline suppression applies only to failure.
	r.changeMu.Lock()
	r.pass = true
	r.changeMu.Unlock()
	passed, stop := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer stop()
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	if reply := receiveCIWatch(t, passed); reply.err != nil || reply.value.State != "passed" || reply.value.Targets[0].WaitingForChange {
		t.Fatal("failure baseline concealed success", reply)
	}
	ciWatchRegistered(t, s, 0)
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) || attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls`) != 0 {
		t.Fatal("baseline watch changed durable state")
	}
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 6 {
		t.Fatal("baseline observers did not share bounded collections", calls)
	}
}

func TestCIWatchObservationKeyIgnoresTimeAndBindsRelevantProof(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "feature"}
	observation := sourceCIAppStatusObservation(target, "failure", false)
	details, err := sourceCIContextDetails(&observation)
	if err != nil {
		t.Fatal(err)
	}
	item := CIWatchItem{State: "failed", Reason: "ci_required_checks_failed", GateRef: "develop", CICommitSHA: target.HeadSHA, Selection: "head", Checks: details}
	original := encoded(observation) + encoded(item)
	key := ciWatchObservationKey("project-one", observation, item)
	later := observation
	later.StartedAt = later.StartedAt.Add(time.Minute)
	later.ObservedAt = later.ObservedAt.Add(time.Minute)
	if ciWatchObservationKey("project-one", later, item) != key || encoded(observation)+encoded(item) != original {
		t.Fatal("timestamp changed key or key changed source")
	}
	for _, mutate := range []func(*reviewprovider.CIObservation, *CIWatchItem){
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Target.RepositoryID++ },
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Target.PullNumber++ },
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Target.HeadSHA = strings.Repeat("c", 40) },
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Target.BaseSHA = strings.Repeat("c", 40) },
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Target.BaseRef = "other" },
		func(_ *reviewprovider.CIObservation, i *CIWatchItem) { i.GateRef = "main" },
		func(_ *reviewprovider.CIObservation, i *CIWatchItem) { i.CICommitSHA = strings.Repeat("c", 40) },
		func(o *reviewprovider.CIObservation, _ *CIWatchItem) { o.Assessment.Checks[0].RunAttempt++ },
		func(_ *reviewprovider.CIObservation, i *CIWatchItem) { i.Checks[0].CommitStatus.ID++ },
		func(_ *reviewprovider.CIObservation, i *CIWatchItem) { i.Checks[0].CheckRun.Conclusion = "failure" },
	} {
		var o reviewprovider.CIObservation
		var i CIWatchItem
		if json.Unmarshal([]byte(encoded(observation)), &o) != nil || json.Unmarshal([]byte(encoded(item)), &i) != nil {
			t.Fatal("copy failed")
		}
		mutate(&o, &i)
		if ciWatchObservationKey("project-one", o, i) == key {
			t.Fatal("relevant changed proof reused key")
		}
	}
	if ciWatchObservationKey("project-two", observation, item) == key {
		t.Fatal("key crossed projects")
	}
	// Provider ordering and unrelated workflow inventory do not constitute a
	// change in the selected required-context outcomes.
	item.Checks = append(item.Checks, SourceCIContext{Context: "other", State: "unproven", Reason: "ci_status_missing"})
	observation.Assessment.Checks = append(observation.Assessment.Checks, reviewprovider.RequiredCIAssessmentCheck{Context: "other", State: "unproven", Reason: "ci_status_missing"})
	ordered := ciWatchObservationKey("project-one", observation, item)
	item.Checks[0], item.Checks[1] = item.Checks[1], item.Checks[0]
	observation.Assessment.Checks[0], observation.Assessment.Checks[1] = observation.Assessment.Checks[1], observation.Assessment.Checks[0]
	observation.Actions.Runs = append(observation.Actions.Runs, reviewprovider.ActionsHeadRun{ID: 999, WorkflowID: 999, Status: "in_progress"})
	if ciWatchObservationKey("project-one", observation, item) != ordered {
		t.Fatal("ordering or optional workflow changed required proof key")
	}
}

func TestCIWatchFailureBaselineNeverSuppressesOtherTerminalFacts(t *testing.T) {
	key := strings.Repeat("d", 64)
	for _, state := range []string{"failed", "passed", "superseded", "closed", "unconfigured"} {
		s := &lifecycleSchedule{}
		sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "one"}, targets: []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), AfterObservationKey: key}}, minimum: []uint64{1}, bases: []string{""}, entries: []*ciWatchEntry{{version: 1, wanted: 1, done: 1, result: CIWatchItem{State: state, ObservationKey: key, Checks: []SourceCIContext{}}}}}
		value := s.ciWatchResult(sub)
		want := state
		if state == "failed" {
			want = "pending"
		}
		if value.State != want || value.Targets[0].State != state || value.Targets[0].WaitingForChange != (state == "failed") {
			t.Fatal("baseline changed other terminal facts", state, value)
		}
	}
}

func TestCIWatchRearmRequiresExplicitBaseBeforeAdmission(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	pin := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), AfterObservationKey: strings.Repeat("c", 64)}
	if _, err := f.e.WatchCI(t.Context(), scope, []CIWatchTarget{pin}, 0); err == nil {
		t.Fatal("engine accepted unpinned rearm")
	}
	body := `{"targets":[{"pull_number":42,"head_sha":"` + pin.HeadSHA + `","after_observation_key":"` + pin.AfterObservationKey + `"}]}`
	req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.reader)
	req.Header.Set("X-FC-CI-Watch", "change-v3")
	req.Header.Set("Prefer", "wait=0")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != 400 || len(s.watchers) != 0 || reader.calls != 0 {
		t.Fatal("HTTP unpinned rearm admitted work", w.Code, w.Body.String())
	}
	pin.BaseSHA = strings.Repeat("b", 40)
	if !validCIWatchTargets([]CIWatchTarget{pin}) {
		t.Fatal("explicit rearm rejected")
	}
}
