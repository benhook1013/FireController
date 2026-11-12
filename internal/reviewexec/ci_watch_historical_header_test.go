package reviewexec

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchHistoricalAttentionRequiresV3Pair(t *testing.T) {
	for _, watch := range []string{"change-v1", "change-v2", "change-v3"} {
		for _, details := range []string{"1", "2", "3"} {
			t.Run(watch+"/"+details, func(t *testing.T) {
				f, s, _, base, _ := ciWatchFixture(t)
				s.readers["test-capability"] = &optionalCIWorkflowAttentionReader{ciWatchWorkflowReader: &ciWatchWorkflowReader{ciWatchReader: base}, available: true}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(fmt.Sprintf(`{"targets":[{"pull_number":42,"head_sha":%q,"failure_workflow_ids":[10]}]}`, strings.Repeat("a", 40)))).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer "+f.reader)
				req.Header.Set("Prefer", "wait=300")
				req.Header.Set("X-FC-CI-Watch", watch)
				req.Header.Set("X-FC-CI-Workflow-Details", details)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); f.handler.ServeHTTP(w, req) }()
				ciWatchRegistered(t, s, 1)
				s.mu.Lock()
				var got CIWatchTarget
				for sub := range s.watchers {
					got = sub.targets[0]
				}
				s.mu.Unlock()
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("HTTP cancellation did not finish")
				}
				wantHead := (watch == "change-v2" || watch == "change-v3") && (details == "2" || details == "3")
				wantHistory := watch == "change-v3" && details == "3"
				if got.AllowHistoricalAttention != wantHistory || got.AllowHeadAttention != wantHead {
					t.Fatal("unsupported capability combination acquired attention", watch, details, got)
				}
				if len(s.watchers) != 0 || base.calls != 0 {
					t.Fatal("header negotiation leaked work")
				}
			})
		}
	}
}

func TestCIWatchBatchOuterStateUsesFirstEligibleTarget(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, attentionFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(attentionFirst), func(t *testing.T) {
			s := &lifecycleSchedule{}
			passed := CIWatchItem{State: "passed", ObservedHeadSHA: head, ObservedBaseSHA: base, CICommitSHA: head, Selection: "head", Checks: []SourceCIContext{}}
			attention := CIWatchItem{State: "pending", ObservedHeadSHA: head, ObservedBaseSHA: base, CICommitSHA: head, Selection: "head", Checks: []SourceCIContext{}, WorkflowFailures: []reviewprovider.CIWorkflowFailure{{WorkflowID: 10, RunID: 100, RunAttempt: 1, JobID: 1000, CheckRunID: 10000, Name: "failed job", Conclusion: "failure"}}}
			items := []CIWatchItem{passed, attention}
			want := "passed"
			index := 1
			if attentionFirst {
				items = []CIWatchItem{attention, passed}
				want = "attention"
				index = 0
			}
			sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "project"}, minimum: []uint64{1, 1}, bases: []string{base, base}}
			for i, item := range items {
				sub.targets = append(sub.targets, CIWatchTarget{PullNumber: int64(42 + i), HeadSHA: head, BaseSHA: base, FailureWorkflowIDs: []int64{10}})
				sub.entries = append(sub.entries, &ciWatchEntry{version: 1, result: item})
			}
			got := s.ciWatchResult(sub)
			if got.State != want || len(got.Targets[index].WorkflowFailures) != 1 || got.Targets[index].State != "pending" {
				t.Fatal("batch lost first wake or later failure", got)
			}
		})
	}
}
