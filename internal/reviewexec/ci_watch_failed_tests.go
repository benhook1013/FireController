package reviewexec

import (
	"context"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const ciWatchFailedTestsBudget = 3 * time.Second

type CIWatchFailedJob struct {
	WorkflowID     int64    `json:"workflow_id"`
	RunID          int64    `json:"run_id"`
	RunAttempt     int64    `json:"run_attempt"`
	JobID          int64    `json:"job_id"`
	CheckRunID     int64    `json:"check_run_id"`
	GateCheckRunID int64    `json:"gate_check_run_id"`
	Tests          []string `json:"tests,omitempty"`
}

type ciFailureNamesReader interface {
	CIFailedTestNames(context.Context, string, int64) []string
}

func ciWatchFailedJobDetails(o reviewprovider.CIObservation, checks []SourceCIContext) []CIWatchFailedJob {
	if o.Actions == nil || o.Actions.HeadSHA != o.SelectedCommitSHA || o.Actions.RepositoryID != o.Target.RepositoryID || !strings.EqualFold(o.Actions.Repository, o.Target.Repository) {
		return nil
	}
	var out []CIWatchFailedJob
	seenJobs := map[int64]bool{}
	seenChecks := map[int64]bool{}
	seenGate := map[int64]bool{}
	for _, check := range checks {
		if len(out) == 3 {
			break
		}
		if check.State != "failed" || check.CheckRun == nil || check.CheckRun.Status != "completed" || check.CheckRun.Conclusion != "failure" || seenGate[check.CheckRun.ID] {
			continue
		}
		gate := check.CheckRun.ID
		seenGate[gate] = true
		var attempt *reviewprovider.ActionsAttempt
		matches := 0
		for i := range o.Attempts {
			a := &o.Attempts[i]
			if !ciWatchCurrentFailureAttempt(o, *a) {
				continue
			}
			for _, j := range a.Jobs {
				if j.CheckRunID != gate {
					continue
				}
				matches++
				if validCIFailedJob(j, o.SelectedCommitSHA) && j.Name == check.Context {
					attempt = a
				}
			}
		}
		if matches != 1 || attempt == nil {
			continue
		}
		// CI may require only an aggregate job. Inspect other failures in the same
		// proven workflow attempt first; that is not a dependency-graph claim.
		ids, checkIDs := map[int64]int{}, map[int64]int{}
		for _, j := range attempt.Jobs {
			ids[j.ID]++
			checkIDs[j.CheckRunID]++
		}
		for _, gateOnly := range []bool{false, true} {
			for _, j := range attempt.Jobs {
				if len(out) == 3 {
					break
				}
				if (j.CheckRunID == gate) != gateOnly || !validCIFailedJob(j, o.SelectedCommitSHA) || ids[j.ID] != 1 || checkIDs[j.CheckRunID] != 1 || seenJobs[j.ID] || seenChecks[j.CheckRunID] {
					continue
				}
				seenJobs[j.ID] = true
				seenChecks[j.CheckRunID] = true
				row := CIWatchFailedJob{WorkflowID: attempt.WorkflowID, RunID: attempt.RunID, RunAttempt: attempt.RunAttempt, JobID: j.ID, CheckRunID: j.CheckRunID, GateCheckRunID: gate}
				out = append(out, row)
			}
		}
	}
	return out
}

func ciWatchCurrentFailureAttempt(o reviewprovider.CIObservation, a reviewprovider.ActionsAttempt) bool {
	if a.State != "complete" || a.HeadSHA != o.SelectedCommitSHA || a.RepositoryID != o.Target.RepositoryID || !strings.EqualFold(a.Repository, o.Target.Repository) || a.RunID <= 0 || a.RunAttempt <= 0 || a.WorkflowID <= 0 || a.CheckSuiteID <= 0 {
		return false
	}
	matches := 0
	for _, r := range o.Actions.Runs {
		if r.ID == a.RunID && r.RunAttempt == a.RunAttempt && r.CheckSuiteID == a.CheckSuiteID && r.WorkflowID == a.WorkflowID && r.RepositoryID == a.RepositoryID && r.HeadSHA == a.HeadSHA && strings.EqualFold(r.Repository, a.Repository) {
			matches++
		}
	}
	return matches == 1
}

func validCIFailedJob(j reviewprovider.ActionsJob, head string) bool {
	return j.ID > 0 && j.CheckRunID > 0 && j.HeadSHA == head && j.Status == "completed" && j.Conclusion == "failure"
}

func (s *lifecycleSchedule) claimCIFailureNames(b ciWatchBinding) (ciFailureNamesReader, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, ok := s.readers[b.capability].(ciFailureNamesReader)
	if !ok || s.ciFailureLogBusy[b.account] {
		return nil, nil
	}
	if s.ciFailureLogBusy == nil {
		s.ciFailureLogBusy = map[string]bool{}
	}
	s.ciFailureLogBusy[b.account] = true
	return reader, func() { s.mu.Lock(); defer s.mu.Unlock(); delete(s.ciFailureLogBusy, b.account) }
}

func enrichCIWatchFailures(ctx context.Context, reader ciFailureNamesReader, item *CIWatchItem) {
	item.FailureDetailState = "unavailable"
	item.FailedJobTests = append([]CIWatchFailedJob(nil), item.FailedJobTests...)
	for i := range item.FailedJobTests {
		if ctx.Err() != nil {
			return
		}
		row := &item.FailedJobTests[i]
		row.Tests = reader.CIFailedTestNames(ctx, item.failedJobRepository, row.JobID)
		if len(row.Tests) > 0 {
			item.FailureDetailState = "available"
		}
	}
}
