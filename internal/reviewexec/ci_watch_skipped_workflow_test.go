package reviewexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchAcceptsOnlyProvenCurrentFailureThroughSkippedRun(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	o := sourceCITestObservation(target, "pending")
	o, row := ciWatchSkippedFailure(o)
	before := encoded(o)
	if rows := ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{row}); len(rows) != 1 || rows[0] != row || encoded(o) != before {
		t.Fatal("valid older current execution hidden or gate facts mutated", rows)
	}
	for _, damage := range []string{"proof", "recheck", "job", "foreign-workflow", "current-attempt-repaired"} {
		t.Run(damage, func(t *testing.T) {
			value, failure := o, row
			switch damage {
			case "proof":
				value.WorkflowAttempts = nil
			case "recheck":
				value.WorkflowActions = nil
			case "job":
				failure.JobID++
			case "foreign-workflow":
				failure.WorkflowID++
			case "current-attempt-repaired":
				actions := *o.WorkflowActions
				actions.Runs = append([]reviewprovider.ActionsHeadRun(nil), actions.Runs...)
				actions.Runs[0].RunAttempt++
				value.WorkflowActions = &actions
			}
			if rows := ciWatchValidatedWorkflowFailures(value, []int64{10}, []reviewprovider.CIWorkflowFailure{failure}); len(rows) != 0 {
				t.Fatal("unproven earlier execution accepted", rows)
			}
		})
	}
}

func ciWatchSkippedFailure(o reviewprovider.CIObservation) (reviewprovider.CIObservation, reviewprovider.CIWorkflowFailure) {
	target := o.Target
	older := reviewprovider.ActionsHeadRun{ID: 100, WorkflowID: 10, CheckSuiteID: 1000, RunNumber: 1, RunAttempt: 1, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: o.SelectedCommitSHA, Event: "pull_request", Status: "completed", Conclusion: "failure", CreatedAt: o.ObservedAt.Add(-2 * time.Minute), UpdatedAt: o.ObservedAt.Add(-time.Minute)}
	newer := older
	newer.ID, newer.CheckSuiteID, newer.RunNumber, newer.Conclusion = 101, 1001, 2, "success"
	newer.CreatedAt, newer.UpdatedAt = o.ObservedAt.Add(-time.Minute), o.ObservedAt
	o.Actions.Runs = append(o.Actions.Runs, older, newer)
	attempt := func(run reviewprovider.ActionsHeadRun, jobID, checkID int64, conclusion string) reviewprovider.ActionsAttempt {
		return reviewprovider.ActionsAttempt{State: "complete", RepositoryID: run.RepositoryID, Repository: run.Repository, HeadSHA: run.HeadSHA, RunID: run.ID, RunNumber: run.RunNumber, RunAttempt: run.RunAttempt, WorkflowID: run.WorkflowID, CheckSuiteID: run.CheckSuiteID, Event: run.Event, Status: run.Status, Conclusion: run.Conclusion, ObservedAt: o.ObservedAt, Jobs: []reviewprovider.ActionsJob{{ID: jobID, CheckRunID: checkID, HeadSHA: run.HeadSHA, Name: "Failed leaf", Status: "completed", Conclusion: conclusion, StartedAt: run.CreatedAt, CompletedAt: run.UpdatedAt}}}
	}
	o.WorkflowAttempts = []reviewprovider.ActionsAttempt{attempt(older, 1000, 10000, "failure"), attempt(newer, 1001, 10001, "skipped")}
	current := *o.Actions
	o.WorkflowActions = &current
	row := reviewprovider.CIWorkflowFailure{WorkflowID: 10, RunID: 100, RunAttempt: 1, JobID: 1000, CheckRunID: 10000, Name: "Failed leaf", Conclusion: "failure"}
	return o, row
}

type ciWatchSkippedReader struct{ *ciWatchReader }

func (r ciWatchSkippedReader) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, gate string, _ []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	o, err := r.ObserveCIGate(ctx, target, gate)
	if err != nil {
		return o, nil, err
	}
	o, row := ciWatchSkippedFailure(o)
	return o, []reviewprovider.CIWorkflowFailure{row}, nil
}

func TestCIWatchSkippedFailureHTTPAcceptedByStrictClient(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	base.states[42] = "pending"
	s.readers["test-capability"] = ciWatchSkippedReader{base}
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'client-reader','Reader')`, attemptProject)
	wire := make(chan string, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			core.WriteJSON(w, 200, map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("X-FC-CI-Workflow-Details") != "2" {
			t.Error("typed client did not negotiate existing optional details")
		}
		recorder := httptest.NewRecorder()
		f.handler.ServeHTTP(recorder, r)
		wire <- recorder.Body.String()
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer service.Close()
	credential := filepath.Join(t.TempDir(), "reader.token")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "client-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	type reply struct {
		value client.CIWatchResult
		err   error
	}
	done := make(chan reply, 1)
	go func() {
		value, err := c.WatchCI(t.Context(), []client.CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10}}}, 5)
		done <- reply{value, err}
	}()
	ciWatchRegistered(t, s, 1)
	watchTick(t, f, s)
	select {
	case got := <-done:
		if got.err != nil || got.value.State != "attention" || len(got.value.Targets) != 1 {
			t.Fatal("real server attention rejected by unchanged strict client", got)
		}
		item := got.value.Targets[0]
		if item.State != "pending" || item.Pending != 1 || item.Failed != 0 || len(item.WorkflowObservations) != 0 || len(item.WorkflowFailures) != 1 || item.WorkflowFailures[0].RunID != 100 || item.WorkflowFailures[0].RunAttempt != 1 {
			t.Fatal("wire projection changed gate or failed execution", item)
		}
		if raw := <-wire; strings.Contains(raw, `"workflow_observations"`) || !json.Valid([]byte(raw)) {
			t.Fatal("conflicting optional details escaped wire", raw)
		}
		out := client.AgentCIWatch(got.value)
		if out.AllTargetsPassed || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "explicitly skipped") || strings.Contains(out.Targets[0].Diagnostic.Explanation, "only the latest") {
			t.Fatal("fallback diagnostic claimed latest-only coverage", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("typed CI watch did not return attention")
	}
	ciWatchRegistered(t, s, 0)
}
