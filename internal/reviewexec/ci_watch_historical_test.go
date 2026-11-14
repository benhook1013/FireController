package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchHistoricalReader struct {
	*ciWatchReader
	normal, historical atomic.Int64
	mixed              bool
}

func ciWatchHistoricalFailure(o reviewprovider.CIObservation) (reviewprovider.CIObservation, reviewprovider.CIWorkflowFailure) {
	o, row := ciWatchSkippedFailure(o)
	row.Basis = reviewprovider.CIWorkflowHistoricalBasis
	row.Name = "Build and Test (account-service / 1)"
	o.WorkflowAttempts[0].Jobs[0].Name = row.Name
	o.WorkflowAttempts[1].Jobs[0].Name = "Build and Test (${{ matrix.module }} / ${{ matrix.partition }})"
	return o, row
}
func (r *ciWatchHistoricalReader) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, gate string, ids []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	r.normal.Add(1)
	o, err := r.ObserveCIGate(ctx, target, gate)
	return o, nil, err
}
func (r *ciWatchHistoricalReader) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gate string, ids, heads, historical []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	o, err := r.ObserveCIGate(ctx, target, gate)
	if err != nil {
		return o, nil, err
	}
	if !slices.Contains(historical, 10) {
		r.normal.Add(1)
		return o, nil, err
	}
	r.historical.Add(1)
	o, row := ciWatchHistoricalFailure(o)
	rows := []reviewprovider.CIWorkflowFailure{row}
	if r.mixed {
		// A second workflow has its own proven same-name skipped-job chain.
		proof, proven := ciWatchSkippedFailure(sourceCITestObservation(target, "pending"))
		for i := range proof.Actions.Runs {
			proof.Actions.Runs[i].ID += 1000
			proof.Actions.Runs[i].CheckSuiteID += 10000
			proof.Actions.Runs[i].WorkflowID = 20
		}
		for i := range proof.WorkflowAttempts {
			a := &proof.WorkflowAttempts[i]
			a.RunID += 1000
			a.CheckSuiteID += 10000
			a.WorkflowID = 20
			a.Jobs[0].ID += 10000
			a.Jobs[0].CheckRunID += 100000
		}
		o.Actions.Runs = append(o.Actions.Runs, proof.Actions.Runs...)
		o.WorkflowAttempts = append(o.WorkflowAttempts, proof.WorkflowAttempts...)
		current := *o.Actions
		o.WorkflowActions = &current
		proven.WorkflowID = 20
		proven.RunID += 1000
		proven.JobID += 10000
		proven.CheckRunID += 100000
		rows = append(rows, proven)
	}
	return o, rows, err
}

func TestCIWatchHistoricalAttentionCoalescesAndProjectsBeforeWakeAndKeys(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchHistoricalReader{ciWatchReader: base}
	s.readers["test-capability"] = r
	old := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}
	v1, cancel1 := startCIWatch(t, f, scope, []CIWatchTarget{old})
	defer cancel1()
	v2pin := old
	v2pin.AllowHeadAttention = true
	v2, cancel2 := startCIWatch(t, f, scope, []CIWatchTarget{v2pin})
	defer cancel2()
	ciWatchRegistered(t, s, 2)
	watchTick(t, f, s)
	if r.normal.Load() != 1 || r.historical.Load() != 0 {
		t.Fatal("old subscriptions requested historical collection", r.normal.Load(), r.historical.Load())
	}
	pin := v2pin
	pin.AllowHistoricalAttention = true
	modern, cancel3 := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancel3()
	ciWatchRegistered(t, s, 3)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	got := receiveCIWatch(t, modern)
	if got.err != nil || got.value.State != "attention" || r.historical.Load() != 1 {
		t.Fatal("new opt-in failed to refresh shared old watch", got, r.historical.Load())
	}
	item := got.value.Targets[0]
	if item.State != "pending" || item.Pending != 1 || item.Failed != 0 || len(item.WorkflowFailures) != 1 || item.WorkflowFailures[0].Basis != reviewprovider.CIWorkflowHistoricalBasis || item.Selection != "head" {
		t.Fatal("historical attention changed gate", item)
	}
	for _, ch := range []<-chan ciWatchReply{v1, v2} {
		select {
		case got := <-ch:
			t.Fatal("history woke older subscriber", got)
		case <-time.After(100 * time.Millisecond):
		}
	}
	ciWatchRegistered(t, s, 2)
	pin.AfterObservationKey = item.ObservationKey
	pin.BaseSHA = item.ObservedBaseSHA
	known, cancelKnown := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancelKnown()
	old.AfterObservationKey = item.ObservationKey
	old.BaseSHA = item.ObservedBaseSHA
	oldKnown, cancelOldKnown := startCIWatch(t, f, scope, []CIWatchTarget{old})
	defer cancelOldKnown()
	ciWatchRegistered(t, s, 4)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	s.mu.Lock()
	var subs []*ciWatchSubscription
	for sub := range s.watchers {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		v := s.ciWatchResult(sub)
		row := v.Targets[0]
		if sub.targets[0].AllowHistoricalAttention {
			if v.State != "pending" || !row.WaitingForChange || len(row.WorkflowFailures) != 1 {
				t.Fatal("historical baseline woke", v)
			}
		} else if v.State != "pending" || row.WaitingForChange || len(row.WorkflowFailures) != 0 || row.ObservationKey == item.ObservationKey {
			t.Fatal("old key/state inherited historical signal", v)
		}
	}
	select {
	case got := <-known:
		t.Fatal("identical historical attention woke", got)
	default:
	}
	select {
	case got := <-oldKnown:
		t.Fatal("old historical key woke", got)
	default:
	}
}

func TestCIWatchHistoricalProjectionKeepsIndependentProvenRows(t *testing.T) {
	f, s, _, base, scope := ciWatchFixture(t)
	base.states[42] = "pending"
	s.readers["test-capability"] = &ciWatchHistoricalReader{ciWatchReader: base, mixed: true}
	pin := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10, 20}}
	old, cancelOld := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancelOld()
	pin.AllowHeadAttention, pin.AllowHistoricalAttention = true, true
	modern, cancelNew := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer cancelNew()
	ciWatchRegistered(t, s, 2)
	watchTick(t, f, s)
	for _, test := range []struct {
		ch       <-chan ciWatchReply
		expected map[int64]string
	}{{old, map[int64]string{20: ""}}, {modern, map[int64]string{10: reviewprovider.CIWorkflowHistoricalBasis, 20: ""}}} {
		got := receiveCIWatch(t, test.ch)
		if got.err != nil || got.value.State != "attention" || len(got.value.Targets[0].WorkflowFailures) != len(test.expected) {
			t.Fatal("mixed projection lost independent proof", test.expected, got)
		}
		for _, failure := range got.value.Targets[0].WorkflowFailures {
			basis, ok := test.expected[failure.WorkflowID]
			if !ok || failure.Basis != basis {
				t.Fatalf("unexpected projected workflow failure: %+v", failure)
			}
			delete(test.expected, failure.WorkflowID)
		}
	}
}

func TestCIWatchHistoricalHTTPStrictClientAndOldDetailVersions(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	base.states[42] = "pending"
	s.readers["test-capability"] = &ciWatchHistoricalReader{ciWatchReader: base}
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'history-reader','Reader')`, attemptProject)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			core.WriteJSON(w, 200, map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	credential := filepath.Join(t.TempDir(), "reader.token")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "history-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		got, err := c.WatchCI(t.Context(), []client.CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}}, 5)
		if err == nil && (got.State != "attention" || len(got.Targets[0].WorkflowFailures) != 1 || got.Targets[0].WorkflowFailures[0].Basis != reviewprovider.CIWorkflowHistoricalBasis) {
			t.Error("strict historical wire lost basis", got)
		}
		done <- err
	}()
	old := map[string]chan *httptest.ResponseRecorder{}
	for _, version := range []string{"1", "2"} {
		response := make(chan *httptest.ResponseRecorder, 1)
		old[version] = response
		go func(version string) {
			r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`","failure_workflow_ids":[10]}]}`))
			r.Header.Set("Authorization", "Bearer "+f.reader)
			r.Header.Set("Prefer", "wait=1")
			r.Header.Set("X-FC-CI-Watch", "change-v2")
			r.Header.Set("X-FC-CI-Workflow-Details", version)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			response <- w
		}(version)
	}
	ciWatchRegistered(t, s, 3)
	watchTick(t, f, s)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("strict historical decode rejected", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("historical request did not wake")
	}
	for _, version := range []string{"1", "2"} {
		var w *httptest.ResponseRecorder
		select {
		case w = <-old[version]:
		case <-time.After(3 * time.Second):
			t.Fatalf("legacy detail version %s did not return", version)
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"timed_out":true`) || strings.Contains(w.Body.String(), "workflow_failures") || strings.Contains(w.Body.String(), reviewprovider.CIWorkflowHistoricalBasis) {
			t.Fatal("old detail shape changed or woke", version, w.Code, w.Body.String())
		}
	}
}

func TestCIWatchHistoricalReaderHonorsWorkflowSelector(t *testing.T) {
	_, _, _, base, _ := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchHistoricalReader{ciWatchReader: base}
	target := reviewprovider.CITarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}
	// Head-only attention must not opt a subscriber into historical failures.
	_, rows, err := r.ObserveCIGateWithWorkflowAttention(t.Context(), target, "main", []int64{10}, []int64{10}, nil)
	if err != nil || len(rows) != 0 {
		t.Fatal("head-only subscriber received historical rows", rows, err)
	}
	for _, ids := range [][]int64{nil, {20}, {10}} {
		_, rows, err := r.ObserveCIGateWithWorkflowAttention(t.Context(), target, "main", ids, ids, ids)
		if err != nil || (len(rows) > 0) != slices.Contains(ids, 10) {
			t.Fatal("historical fixture ignored selector", ids, rows, err)
		}
	}
}
