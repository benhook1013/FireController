package reviewexec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchHeadAttentionReader struct {
	*ciWatchReader
	ordinary, head atomic.Int64
}

func (r *ciWatchHeadAttentionReader) ObserveCIGate(_ context.Context, target reviewprovider.CITarget, _ string) (reviewprovider.CIObservation, error) {
	r.ordinary.Add(1)
	return testCIMismatchObservation(target), nil
}
func (r *ciWatchHeadAttentionReader) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, gate string, _ []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	o, err := r.ObserveCIGate(ctx, target, gate)
	return o, nil, err
}
func (r *ciWatchHeadAttentionReader) ObserveCIGateWithWorkflowAttention(_ context.Context, target reviewprovider.CITarget, _ string, _, headIDs []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	r.head.Add(1)
	o := testCIMismatchObservation(target)
	for _, id := range headIDs {
		if id == 10 {
			proof, row := ciWatchSkippedFailure(sourceCITestObservation(target, "pending"))
			o.WorkflowHeadActions, o.WorkflowActions, o.WorkflowAttempts = proof.Actions, proof.WorkflowActions, proof.WorkflowAttempts
			return o, []reviewprovider.CIWorkflowFailure{row}, nil
		}
	}
	return o, nil, nil
}

func TestCIWatchHeadAttentionCoalescesWithoutChangingLegacyWake(t *testing.T) {
	f, s, clock, base, scope := ciWatchFixture(t)
	r := &ciWatchHeadAttentionReader{ciWatchReader: base}
	s.readers["test-capability"] = r
	head := strings.Repeat("a", 40)
	legacyPin := CIWatchTarget{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}}
	legacy, stopLegacy := startCIWatch(t, f, scope, []CIWatchTarget{legacyPin})
	defer stopLegacy()
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	if r.ordinary.Load() != 1 || r.head.Load() != 0 {
		t.Fatal("old-only selection added head probes", r.ordinary.Load(), r.head.Load())
	}
	select {
	case got := <-legacy:
		t.Fatal("legacy mismatch unexpectedly woke", got)
	default:
	}
	// Same IDs, but the new capable subscriber must request fresh proof.
	pin := legacyPin
	pin.AllowHeadAttention = true
	current, stopCurrent := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer stopCurrent()
	ciWatchRegistered(t, s, 2)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	got := receiveCIWatch(t, current)
	if got.err != nil || got.value.State != "attention" || r.head.Load() != 1 {
		t.Fatal("new opt-in joining old watch missed fresh attention", got, r.head.Load())
	}
	item := got.value.Targets[0]
	if item.WorkflowAttentionBasis != reviewprovider.CIWorkflowHeadOnlyBasis || item.Reason != "ci_merge_base_mismatch" || item.State != "pending" || item.Required != 0 || item.Failed != 0 || len(item.Checks) != 0 || item.Selection != "" || item.CICommitSHA != "" || len(item.WorkflowFailures) != 1 {
		t.Fatal("same-head attention changed gate or source proof", item)
	}
	select {
	case got := <-legacy:
		t.Fatal("head-only attention woke legacy subscriber", got)
	default:
	}
	ciWatchRegistered(t, s, 1)
	// Identical head-only attention is quiet for a new caller's baseline; an
	// old caller with that same key must still receive no failure or wake.
	pin.AfterObservationKey = item.ObservationKey
	waiting, stopWaiting := startCIWatch(t, f, scope, []CIWatchTarget{pin})
	defer stopWaiting()
	legacyPin.AfterObservationKey = item.ObservationKey
	oldBaseline, stopOldBaseline := startCIWatch(t, f, scope, []CIWatchTarget{legacyPin})
	defer stopOldBaseline()
	ciWatchRegistered(t, s, 3)
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
		if sub.targets[0].AllowHeadAttention {
			if v.State != "pending" || !row.WaitingForChange || len(row.WorkflowFailures) != 1 {
				t.Fatal("same head attention baseline did not stay quiet", v)
			}
		} else if v.State != "pending" || row.WaitingForChange || row.WorkflowAttentionBasis != "" || len(row.WorkflowFailures) != 0 {
			t.Fatal("legacy baseline inherited head-only signal", v)
		}
	}
	select {
	case got := <-waiting:
		t.Fatal("identical head-only attention woke", got)
	default:
	}
	select {
	case got := <-oldBaseline:
		t.Fatal("legacy baseline woke", got)
	default:
	}
}

func TestCIWatchHeadAttentionRealHTTPStrictClientAndLegacyProjection(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	s.readers["test-capability"] = &ciWatchHeadAttentionReader{ciWatchReader: base}
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'head-reader','Reader')`, attemptProject)
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
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "head-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	type reply struct {
		value client.CIWatchResult
		err   error
	}
	modern := make(chan reply, 1)
	go func() {
		v, e := c.WatchCI(t.Context(), []client.CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}}, 5)
		modern <- reply{v, e}
	}()
	legacy := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`","failure_workflow_ids":[10]}]}`))
		r.Header.Set("Authorization", "Bearer "+f.reader)
		r.Header.Set("Prefer", "wait=1")
		r.Header.Set("X-FC-CI-Watch", "change-v1")
		r.Header.Set("X-FC-CI-Workflow-Details", "1")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		legacy <- w
	}()
	ciWatchRegistered(t, s, 2)
	watchTick(t, f, s)
	select {
	case got := <-modern:
		if got.err != nil || got.value.State != "attention" || got.value.Targets[0].WorkflowAttentionBasis != reviewprovider.CIWorkflowHeadOnlyBasis {
			t.Fatal("strict client rejected head-only attention", got)
		}
		out := client.AgentCIWatch(got.value)
		if out.AllTargetsPassed || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "base/merge comparison is unknown") || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "not a current-comparison failure") {
			t.Fatal("agent diagnostic claimed current CI or a fabricated run base", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("modern attention did not return")
	}
	w := <-legacy
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"pending"`) || !strings.Contains(w.Body.String(), `"timed_out":true`) || strings.Contains(w.Body.String(), "workflow_attention_basis") || strings.Contains(w.Body.String(), "workflow_failures") {
		t.Fatal("old strict wire changed or woke on head-only failure", w.Code, w.Body.String())
	}
}
