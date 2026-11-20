package reviewexec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type fakeCIFailedNames struct{ jobs []int64 }

func (f *fakeCIFailedNames) CIFailedTestNames(_ context.Context, repo string, job int64) []string {
	if repo != "owner/repo" {
		return nil
	}
	f.jobs = append(f.jobs, job)
	return []string{"TestOrdinary"}
}
func TestCIWatchFailedTestsUseOnlyExactFailedAttempt(t *testing.T) {
	head := strings.Repeat("a", 40)
	const repo = "owner/repo"
	o := reviewprovider.CIObservation{SelectedCommitSHA: head, Target: reviewprovider.CITarget{Repository: repo, RepositoryID: 55}}
	o.Actions = &reviewprovider.ActionsHeadInventory{Repository: repo, RepositoryID: 55, HeadSHA: head, Runs: []reviewprovider.ActionsHeadRun{{ID: 5, RunAttempt: 2, Repository: repo, RepositoryID: 55, HeadSHA: head, CheckSuiteID: 6, WorkflowID: 7}}}
	a := reviewprovider.ActionsAttempt{Repository: repo, RepositoryID: 55, HeadSHA: head, State: "complete", RunID: 5, RunAttempt: 2, CheckSuiteID: 6, WorkflowID: 7, Jobs: []reviewprovider.ActionsJob{{ID: 12, CheckRunID: 34, HeadSHA: head, Name: "go test", Status: "completed", Conclusion: "failure"}}}
	old := a
	old.RunAttempt = 1
	old.Jobs = append([]reviewprovider.ActionsJob(nil), a.Jobs...)
	old.Jobs[0].ID = 13
	a.Jobs = append(a.Jobs, reviewprovider.ActionsJob{ID: 33, CheckRunID: 45, HeadSHA: head, Name: "race shard", Status: "completed", Conclusion: "failure"})
	o.Attempts = []reviewprovider.ActionsAttempt{old, a}
	check := SourceCIContext{Context: "go test", State: "failed", CheckRun: &SourceCICheckRun{ID: 34, Status: "completed", Conclusion: "failure"}}
	f := &fakeCIFailedNames{}
	got := ciWatchFailedJobDetails(o, []SourceCIContext{check})
	item := CIWatchItem{failedJobRepository: repo, FailedJobTests: got}
	enrichCIWatchFailures(t.Context(), f, &item)
	if len(got) != 2 || got[0].RunAttempt != 2 || got[0].CheckRunID != 45 || got[0].GateCheckRunID != 34 || got[0].Tests != nil || fmt.Sprint(f.jobs) != "[33 12]" || fmt.Sprint(item.FailedJobTests[0].Tests) != "[TestOrdinary]" {
		t.Fatal(got, f.jobs)
	}
	o.Attempts = append(o.Attempts, a)
	got = ciWatchFailedJobDetails(o, []SourceCIContext{check})
	if len(got) != 0 || len(f.jobs) != 2 {
		t.Fatal("ambiguous check fetched", got, f.jobs)
	}
}

type ciWatchPausedLogReader struct {
	*ciWatchReader
	entered chan struct{}
	release chan struct{}
}

func (r *ciWatchPausedLogReader) CIFailedTestNames(ctx context.Context, _ string, _ int64) []string {
	close(r.entered)
	select {
	case <-r.release:
		return []string{"TestFailed"}
	case <-ctx.Done():
		return nil
	}
}

func TestCIWatchFailedTestsDoesNotHoldOtherWatchers(t *testing.T) {
	f, s, _, plain, scope := ciWatchFixture(t)
	reader := &ciWatchPausedLogReader{ciWatchReader: plain, entered: make(chan struct{}), release: make(chan struct{})}
	s.readers["test-capability"] = reader
	pull := lifecyclePull(42)
	opted, cancelOpted := startCIWatch(t, f, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: pull.HeadSHA, IncludeFailedTests: true}})
	defer cancelOpted()
	// Deliberately give the non-opted caller a short independent deadline.
	legacyCtx, cancelLegacy := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelLegacy()
	legacy := make(chan ciWatchReply, 1)
	go func() {
		v, err := f.e.WatchCI(legacyCtx, scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: pull.HeadSHA}}, 30)
		legacy <- ciWatchReply{v, err}
	}()
	ciWatchRegistered(t, s, 2)
	// Publish a proven failure tuple, as the provider observer does, without
	// running a fixture log read on the shared observer goroutine.
	s.mu.Lock()
	for _, e := range s.watches {
		e.version = 1
		e.result = CIWatchItem{State: "failed", Reason: "ci_required_checks_failed", ObservedHeadSHA: pull.HeadSHA, ObservedBaseSHA: pull.BaseSHA, failedJobRepository: "owner/repo", FailedJobTests: []CIWatchFailedJob{{WorkflowID: 1, RunID: 2, RunAttempt: 3, JobID: 4, CheckRunID: 5, GateCheckRunID: 6}}}
		for sub := range e.owners {
			sub.changed <- struct{}{}
		}
	}
	s.mu.Unlock()
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic did not enter")
	}
	if result := receiveCIWatch(t, legacy); result.err != nil || result.value.State != "failed" || len(result.value.Targets[0].FailedJobTests) != 0 {
		t.Fatal(result)
	}
	close(reader.release)
	if result := receiveCIWatch(t, opted); result.err != nil || result.value.State != "failed" || result.value.Targets[0].FailureDetailState != "available" || fmt.Sprint(result.value.Targets[0].FailedJobTests[0].Tests) != "[TestFailed]" {
		t.Fatal(result)
	}
}
