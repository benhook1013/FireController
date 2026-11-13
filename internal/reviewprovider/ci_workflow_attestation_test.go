package reviewprovider

import "testing"

func TestCIWorkflowLatestFailureRequiresRetainedJobProof(t *testing.T) {
	f := ciWorkflowFailureFixture(t, "optional")
	got, rows, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
	if err != nil || len(rows) != 1 || !CIWorkflowFailureApplies(got, rows[0]) {
		t.Fatal(rows, err)
	}
	for _, kind := range []string{"job", "check", "name", "conclusion", "no attempt", "duplicate attempt", "successful job", "missing jobs"} {
		t.Run(kind, func(t *testing.T) {
			o, row := got, rows[0]
			o.WorkflowAttempts = append([]ActionsAttempt(nil), got.WorkflowAttempts...)
			switch kind {
			case "job":
				row.JobID++
			case "check":
				row.CheckRunID++
			case "name":
				row.Name = "Other job"
			case "conclusion":
				row.Conclusion = "timed_out"
			case "no attempt":
				o.WorkflowAttempts = nil
			case "duplicate attempt":
				o.WorkflowAttempts = append(o.WorkflowAttempts, o.WorkflowAttempts[0])
			case "successful job":
				o.WorkflowAttempts[0].Jobs = append([]ActionsJob(nil), o.WorkflowAttempts[0].Jobs...)
				for i := range o.WorkflowAttempts[0].Jobs {
					o.WorkflowAttempts[0].Jobs[i].Conclusion = "success"
				}
			case "missing jobs":
				o.WorkflowAttempts[0].Jobs = nil
			}
			if CIWorkflowFailureApplies(o, row) {
				t.Fatal("unsupported latest job accepted", kind)
			}
		})
	}
}

func TestCIWorkflowLatestFailureHonorsSuccessfulRecheck(t *testing.T) {
	f := ciSkippedRunFixture(t, "mixed stable")
	got, rows, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31, 32})
	if err != nil || got.WorkflowActions == nil {
		t.Fatal(err)
	}
	var direct CIWorkflowFailure
	for _, r := range rows {
		if r.WorkflowID == 32 {
			direct = r
		}
	}
	if direct.JobID == 0 || !CIWorkflowFailureApplies(got, direct) {
		t.Fatal("fixture has no proven direct failure", rows)
	}
	for _, kind := range []string{"stable", "repaired", "new run", "foreign head", "failed recheck"} {
		t.Run(kind, func(t *testing.T) {
			o := got
			current := *got.WorkflowActions
			current.Runs = append([]ActionsHeadRun(nil), current.Runs...)
			o.WorkflowActions = &current
			switch kind {
			case "repaired":
				for i := range current.Runs {
					if current.Runs[i].WorkflowID == 32 {
						current.Runs[i].RunAttempt++
						current.Runs[i].Conclusion = "success"
					}
				}
			case "new run":
				for i := range current.Runs {
					if current.Runs[i].WorkflowID == 32 {
						current.Runs[i].ID++
						current.Runs[i].CheckSuiteID++
						current.Runs[i].RunNumber++
					}
				}
			case "foreign head":
				current.HeadSHA = ciObservationTarget().HeadSHA
			case "failed recheck":
				o.WorkflowActions = nil
			}
			if actual, want := CIWorkflowFailureApplies(o, direct), kind == "stable" || kind == "failed recheck"; actual != want {
				t.Fatal("successful recheck or failed-recheck allowance ignored", kind, actual)
			}
		})
	}
}
