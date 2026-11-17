package reviewexec

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestCIWatchRepeatedUnregisterPreservesPrincipalAccounting(t *testing.T) {
	_, s, _, _, scope := ciWatchFixture(t)
	binding := ciWatchBinding{capability: "test-capability"}
	target := []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}
	a, err := s.registerCIWatch(scope, binding, target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.registerCIWatch(scope, binding, target)
	if err != nil {
		t.Fatal(err)
	}
	s.unregisterCIWatch(a)
	s.unregisterCIWatch(a)
	if s.watchPrincipals[scope.PrincipalID] != 1 || !s.watchers[b] || len(s.watches) != 1 {
		t.Fatal("repeated unregister removed live capacity", s.watchPrincipals)
	}
	s.unregisterCIWatch(b)
	s.unregisterCIWatch(b)
	if len(s.watchPrincipals) != 0 || len(s.watchers) != 0 || len(s.watches) != 0 {
		t.Fatal("cleanup leaked registration")
	}
}

func TestCIWatchResponseBoundUsesNegotiatedProjection(t *testing.T) {
	for _, mode := range []string{"http-omitted", "http-details", "direct"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _, base, scope := ciWatchFixture(t)
			s.readers["test-capability"] = &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{}}
			head, baseSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
			done := make(chan struct{})
			var gotErr error
			w := httptest.NewRecorder()
			go func() {
				defer close(done)
				if mode == "direct" {
					_, gotErr = f.e.WatchCI(context.Background(), scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10}}}, 1)
					return
				}
				r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+head+`","failure_workflow_ids":[10]}]}`))
				r.Header.Set("Authorization", "Bearer "+f.reader)
				r.Header.Set("Prefer", "wait=1")
				r.Header.Set("X-FC-CI-Watch", "change-v1")
				if mode == "http-details" {
					r.Header.Set("X-FC-CI-Workflow-Details", "1")
				}
				f.handler.ServeHTTP(w, r)
			}()
			ciWatchRegistered(t, s, 1)
			s.mu.Lock()
			for sub := range s.watchers {
				entry := sub.entries[0]
				entry.version = sub.minimum[0]
				entry.wanted = 1
				entry.done = 1
				entry.result = CIWatchItem{State: "pending", Reason: "ci_required_checks_pending", ObservedHeadSHA: head, ObservedBaseSHA: baseSHA, CICommitSHA: head, Selection: "head", Checks: []SourceCIContext{}, WorkflowObservations: []CIWorkflowObservation{{WorkflowID: 10, Status: strings.Repeat("x", 513<<10)}}}
			}
			s.mu.Unlock()
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("watch failed to return")
			}
			switch mode {
			case "http-omitted":
				if w.Code != 200 || strings.Contains(w.Body.String(), "workflow_observations") {
					t.Fatal(w.Code, w.Body.String())
				}
			case "http-details":
				if w.Code != 503 || !strings.Contains(w.Body.String(), "ci_watch_response_limit") {
					t.Fatal(w.Code, w.Body.String())
				}
			case "direct":
				var failure *core.Error
				if !errors.As(gotErr, &failure) || failure.Code != "ci_watch_response_limit" {
					t.Fatal(gotErr)
				}
			}
			ciWatchRegistered(t, s, 0)
		})
	}
}
