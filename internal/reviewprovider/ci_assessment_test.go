package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type ciAssessmentFixture struct {
	Authority RequiredCheckAuthority
	Inventory CIResultInventory
	Actions   ActionsHeadInventory
	Attempts  []ActionsAttempt
}

func requiredCIFixture() ciAssessmentFixture {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	head := strings.Repeat("a", 40)
	run := ActionsHeadRun{ID: 80, CheckSuiteID: 90, WorkflowID: 100, RunNumber: 12, RunAttempt: 2, RepositoryID: 200, Repository: "Owner/Repo", HeadSHA: head, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	check := CICheckRun{ID: 300, SuiteID: 90, AppID: 400, Name: "build", HeadSHA: head, Status: "completed", Conclusion: "success", StartedAt: now.Add(-time.Minute), CompletedAt: now}
	job := ActionsJob{ID: 500, CheckRunID: 300, Name: "renamed display label", HeadSHA: head, Status: "completed", Conclusion: "success", StartedAt: check.StartedAt, CompletedAt: now}
	return ciAssessmentFixture{Authority: RequiredCheckAuthority{State: "complete", Repository: "owner/repo", Branch: "main", ObservedAt: now, Checks: []RequiredCheck{{Context: "build", AppID: 400}}}, Inventory: CIResultInventory{Repository: "owner/repo", HeadSHA: head, ObservedAt: now, Checks: []CICheckRun{check}, Statuses: []CICommitStatus{}}, Actions: ActionsHeadInventory{Repository: "owner/repo", RepositoryID: 200, HeadSHA: head, ObservedAt: now, Runs: []ActionsHeadRun{run}}, Attempts: []ActionsAttempt{{State: "complete", Repository: "owner/repo", RepositoryID: 200, HeadSHA: head, CheckSuiteID: 90, RunID: 80, WorkflowID: 100, RunNumber: 12, RunAttempt: 2, Event: "pull_request", Status: "completed", Conclusion: "success", ObservedAt: now, Jobs: []ActionsJob{job}}}}
}
func (f ciAssessmentFixture) assess() (RequiredCIAssessment, error) {
	return AssessRequiredCI(f.Authority, f.Inventory, f.Actions, f.Attempts)
}
func fixtureJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func addCIStatus(f *ciAssessmentFixture, id int64, state string) {
	f.Inventory.Statuses = append(f.Inventory.Statuses, CICommitStatus{ID: id, Context: "build", State: state, CreatedAt: f.Inventory.ObservedAt, UpdatedAt: f.Inventory.ObservedAt})
}
func setCIJobState(f *ciAssessmentFixture, status, conclusion string) {
	f.Inventory.Checks[0].Status, f.Inventory.Checks[0].Conclusion = status, conclusion
	f.Attempts[0].Jobs[0].Status, f.Attempts[0].Jobs[0].Conclusion = status, conclusion
	if status != "completed" {
		f.Inventory.Checks[0].CompletedAt = time.Time{}
		f.Attempts[0].Jobs[0].CompletedAt = time.Time{}
	}
}

func TestAssessRequiredCIExactCurrentJobAndOutcomeMatrix(t *testing.T) {
	tests := []struct{ name, status, conclusion, want string }{
		{"success", "completed", "success", "satisfied"}, {"neutral", "completed", "neutral", "satisfied"}, {"skipped", "completed", "skipped", "satisfied"},
		{"failure", "completed", "failure", "failed"}, {"cancelled", "completed", "cancelled", "failed"}, {"timedout", "completed", "timed_out", "failed"}, {"action", "completed", "action_required", "failed"}, {"startup", "completed", "startup_failure", "failed"}, {"stale", "completed", "stale", "failed"},
		{"queued", "queued", "", "pending"}, {"running", "in_progress", "", "pending"}, {"waiting", "waiting", "", "pending"}, {"pending", "pending", "", "pending"}, {"requested", "requested", "", "pending"}, {"futureconclusion", "completed", "unrecognized", "unproven"}, {"futurestate", "new_provider_state", "", "unproven"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := requiredCIFixture()
			setCIJobState(&f, tt.status, tt.conclusion)
			before := string(fixtureJSON(t, f))
			v, err := f.assess()
			if err != nil || v.State != tt.want || len(v.Checks) != 1 || v.Checks[0].State != tt.want || v.Checks[0].CheckRunID != 300 || v.Checks[0].RunID != 80 || v.Checks[0].RunAttempt != 2 {
				t.Fatalf("wrong current result: %+v %v", v, err)
			}
			if string(fixtureJSON(t, f)) != before {
				t.Fatal("input mutated")
			}
			if tt.want == "satisfied" && v.Checks[0].Reason != "" {
				t.Fatal(v)
			}
		})
	}
}
func TestAssessRequiredCICurrentAttemptIdentityCannotBeGuessed(t *testing.T) {
	cases := map[string]func(*ciAssessmentFixture){
		"missingattempt": func(f *ciAssessmentFixture) { f.Attempts = []ActionsAttempt{} },
		"unprovenattempt": func(f *ciAssessmentFixture) {
			f.Attempts[0] = ActionsAttempt{State: "unproven", Repository: "owner/repo", HeadSHA: f.Inventory.HeadSHA, CheckSuiteID: 90, ObservedAt: f.Inventory.ObservedAt, Jobs: []ActionsJob{}}
		},
		"partialrerunmissingjob": func(f *ciAssessmentFixture) { f.Attempts[0].Jobs = []ActionsJob{} },
		"nameisnotidentity": func(f *ciAssessmentFixture) {
			f.Attempts[0].Jobs[0].Name = "build"
			f.Attempts[0].Jobs[0].CheckRunID = 301
		},
		"nonactionsunknownsuite": func(f *ciAssessmentFixture) { f.Inventory.Checks[0].SuiteID = 999 },
		"differentapp":           func(f *ciAssessmentFixture) { f.Inventory.Checks[0].AppID = 401 },
		"contextcase":            func(f *ciAssessmentFixture) { f.Inventory.Checks[0].Name = "Build" },
		"checkjobstatusmovement": func(f *ciAssessmentFixture) { f.Inventory.Checks[0].Conclusion = "failure" },
		"manualdispatch": func(f *ciAssessmentFixture) {
			f.Actions.Runs[0].Event = "workflow_dispatch"
			f.Attempts[0].Event = "workflow_dispatch"
		},
		"workflowrun": func(f *ciAssessmentFixture) {
			f.Actions.Runs[0].Event = "workflow_run"
			f.Attempts[0].Event = "workflow_run"
		},
		"twocurrentmatchingjobs": func(f *ciAssessmentFixture) {
			c := f.Inventory.Checks[0]
			c.ID = 301
			f.Inventory.Checks = append(f.Inventory.Checks, c)
			j := f.Attempts[0].Jobs[0]
			j.ID, j.CheckRunID = 501, 301
			f.Attempts[0].Jobs = append(f.Attempts[0].Jobs, j)
		},
		"unknownoriginbesidecurrent": func(f *ciAssessmentFixture) {
			c := f.Inventory.Checks[0]
			c.ID, c.SuiteID = 301, 999
			f.Inventory.Checks = append(f.Inventory.Checks, c)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := requiredCIFixture()
			edit(&f)
			v, err := f.assess()
			if err != nil || v.State != "unproven" || len(v.Checks) != 1 {
				t.Fatalf("unsafe credit %+v %v", v, err)
			}
		})
	}
}
func TestAssessRequiredCILatestRunOverAllEventsNotOlderRerun(t *testing.T) {
	for _, event := range []string{"pull_request", "workflow_dispatch", "workflow_run"} {
		for _, status := range []string{"queued", "completed"} {
			t.Run(event+status, func(t *testing.T) {
				f := requiredCIFixture()
				f.Actions.Runs[0].RunAttempt = 99
				f.Attempts[0].RunAttempt = 99
				newest := f.Actions.Runs[0]
				newest.ID, newest.CheckSuiteID, newest.RunNumber, newest.RunAttempt = 1, 2, 13, 1
				newest.Event, newest.Status = event, status
				if status == "queued" {
					newest.Conclusion = ""
				} else {
					newest.Conclusion = "failure"
				}
				f.Actions.Runs = append(f.Actions.Runs, newest)
				// The older green check is positively obsolete, despite a higher ID,
				// higher rerun attempt and later timestamp. No current job is available.
				for _, reverse := range []bool{false, true} {
					if reverse {
						f.Actions.Runs[0], f.Actions.Runs[1] = f.Actions.Runs[1], f.Actions.Runs[0]
					}
					v, err := f.assess()
					if err != nil || v.State != "unproven" || v.Checks[0].CheckRunID != 0 {
						t.Fatalf("older run gained credit %+v %v", v, err)
					}
				}
			})
		}
	}
	// An explicitly older run cannot poison a proven current matching check.
	f := requiredCIFixture()
	old := f.Actions.Runs[0]
	old.ID, old.CheckSuiteID, old.RunNumber = 900, 901, 11
	old.RunAttempt = 99
	f.Actions.Runs = append(f.Actions.Runs, old)
	c := f.Inventory.Checks[0]
	c.ID, c.SuiteID, c.Conclusion = 999, 901, "failure"
	f.Inventory.Checks = append([]CICheckRun{c}, f.Inventory.Checks...)
	v, err := f.assess()
	if err != nil || v.State != "satisfied" || v.Checks[0].CheckRunID != 300 {
		t.Fatalf("proven old run not ignored %+v %v", v, err)
	}
}
func TestAssessRequiredCIStatusesUseProviderOrderAndBothKinds(t *testing.T) {
	for _, first := range []string{"success", "failure", "error", "pending", "future"} {
		t.Run(first, func(t *testing.T) {
			f := requiredCIFixture()
			f.Authority.Checks[0].AppID = 0
			f.Inventory.Checks = []CICheckRun{}
			addCIStatus(&f, 3, first)
			addCIStatus(&f, 999, "success")
			f.Inventory.Statuses[1].UpdatedAt = f.Inventory.Statuses[1].UpdatedAt.Add(time.Hour)
			want := map[string]string{"success": "satisfied", "failure": "failed", "error": "failed", "pending": "pending", "future": "unproven"}[first]
			v, err := f.assess()
			if err != nil || v.State != want || v.Checks[0].StatusID != 3 || v.Checks[0].CheckRunID != 0 {
				t.Fatalf("status precedence wrong %+v %v", v, err)
			}
		})
	}
	f := requiredCIFixture()
	f.Authority.Checks[0].AppID = 0
	addCIStatus(&f, 1, "failure")
	v, err := f.assess()
	if err != nil || v.State != "failed" || v.Checks[0].CheckRunID != 300 || v.Checks[0].StatusID != 1 {
		t.Fatal(v, err)
	}
	f.Inventory.Statuses[0].State = "pending"
	v, err = f.assess()
	if err != nil || v.State != "pending" {
		t.Fatal(v, err)
	}
	f.Inventory.Statuses[0].State = "success"
	setCIJobState(&f, "completed", "failure")
	v, err = f.assess()
	if err != nil || v.State != "failed" {
		t.Fatal(v, err)
	}
	setCIJobState(&f, "in_progress", "")
	v, err = f.assess()
	if err != nil || v.State != "pending" {
		t.Fatal(v, err)
	}
	f.Authority.Checks[0].AppID = 400
	v, err = f.assess()
	if err != nil || v.State != "pending" || v.Checks[0].Reason != "ci_check_pending" {
		t.Fatal(v, err)
	}
}
func TestAssessRequiredCIAggregationAndExplicitEmptyAuthority(t *testing.T) {
	f := requiredCIFixture()
	f.Authority.Checks = []RequiredCheck{}
	v, err := f.assess()
	if err != nil || v.State != "not_required" || v.Checks == nil || len(v.Checks) != 0 {
		t.Fatal(v, err)
	}
	f.Authority.Checks = nil
	v, err = f.assess()
	if err != nil || v.State != "unproven" {
		t.Fatal(v, err)
	}
	f = requiredCIFixture()
	f.Authority.State = "unproven"
	v, err = f.assess()
	if err != nil || v.State != "unproven" {
		t.Fatal(v, err)
	}
	f = requiredCIFixture()
	f.Inventory.Checks = nil
	v, err = f.assess()
	if err != nil || v.State != "unproven" {
		t.Fatal(v, err)
	}
	f = requiredCIFixture()
	f.Actions.Runs = nil
	v, err = f.assess()
	if err != nil || v.State != "unproven" {
		t.Fatal(v, err)
	}
	f = requiredCIFixture()
	f.Authority.Checks = append(f.Authority.Checks, RequiredCheck{Context: "status", AppID: 0})
	f.Inventory.Statuses = []CICommitStatus{{ID: 1, Context: "status", State: "failure", CreatedAt: f.Inventory.ObservedAt, UpdatedAt: f.Inventory.ObservedAt}}
	setCIJobState(&f, "queued", "")
	v, err = f.assess()
	if err != nil || v.State != "failed" || v.Checks[0].State != "pending" || v.Checks[1].State != "failed" {
		t.Fatal(v, err)
	}
	f.Authority.Checks = append(f.Authority.Checks, RequiredCheck{Context: "absent", AppID: 0})
	v, err = f.assess()
	if err != nil || v.State != "unproven" {
		t.Fatal(v, err)
	}
}
func TestAssessRequiredCIRejectsMalformedForeignAndConflictingFacts(t *testing.T) {
	cases := map[string]func(*ciAssessmentFixture){
		"foreignrepo":                    func(f *ciAssessmentFixture) { f.Inventory.Repository = "foreign/repo" },
		"foreignhead":                    func(f *ciAssessmentFixture) { f.Inventory.Checks[0].HeadSHA = strings.Repeat("b", 40) },
		"foreignattemptrepo":             func(f *ciAssessmentFixture) { f.Attempts[0].Repository = "foreign/repo" },
		"foreignattempthead":             func(f *ciAssessmentFixture) { f.Attempts[0].HeadSHA = strings.Repeat("b", 40) },
		"foreignrepositoryid":            func(f *ciAssessmentFixture) { f.Attempts[0].RepositoryID++ },
		"obsoleteattempt":                func(f *ciAssessmentFixture) { f.Attempts[0].RunAttempt = 1 },
		"differentworkflow":              func(f *ciAssessmentFixture) { f.Attempts[0].WorkflowID++ },
		"differentrun":                   func(f *ciAssessmentFixture) { f.Attempts[0].RunID++ },
		"differentnumber":                func(f *ciAssessmentFixture) { f.Attempts[0].RunNumber++ },
		"differentevent":                 func(f *ciAssessmentFixture) { f.Attempts[0].Event = "push" },
		"differentrunstatus":             func(f *ciAssessmentFixture) { f.Attempts[0].Conclusion = "failure" },
		"checkduplicate":                 func(f *ciAssessmentFixture) { f.Inventory.Checks = append(f.Inventory.Checks, f.Inventory.Checks[0]) },
		"statusduplicate":                func(f *ciAssessmentFixture) { addCIStatus(f, 1, "success"); addCIStatus(f, 1, "failure") },
		"jobduplicate":                   func(f *ciAssessmentFixture) { f.Attempts[0].Jobs = append(f.Attempts[0].Jobs, f.Attempts[0].Jobs[0]) },
		"attemptduplicate":               func(f *ciAssessmentFixture) { f.Attempts = append(f.Attempts, f.Attempts[0]) },
		"requiredduplicate":              func(f *ciAssessmentFixture) { f.Authority.Checks = append(f.Authority.Checks, f.Authority.Checks[0]) },
		"completeauthoritycontradiction": func(f *ciAssessmentFixture) { f.Authority.Reason = "authority_unavailable" },
		"completeattemptcontradiction":   func(f *ciAssessmentFixture) { f.Attempts[0].Reason = "ci_attempt_changed" },
		"negativeappid":                  func(f *ciAssessmentFixture) { f.Authority.Checks[0].AppID = -1 },
		"badchecktime": func(f *ciAssessmentFixture) {
			f.Inventory.Checks[0].CompletedAt = f.Inventory.Checks[0].StartedAt.Add(-time.Second)
		},
		"emptyjobsnotcomplete": func(f *ciAssessmentFixture) { f.Attempts[0].Jobs = nil },
		"badjobhead":           func(f *ciAssessmentFixture) { f.Attempts[0].Jobs[0].HeadSHA = strings.Repeat("b", 40) },
		"unboundedcontext":     func(f *ciAssessmentFixture) { f.Authority.Checks[0].Context = strings.Repeat("x", 256) },
		"unboundedcollection":  func(f *ciAssessmentFixture) { f.Inventory.Statuses = make([]CICommitStatus, githubMaxItems+1) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := requiredCIFixture()
			edit(&f)
			v, err := f.assess()
			if err == nil || v.State != "unproven" {
				t.Fatalf("malformed facts gained credit %+v %v", v, err)
			}
		})
	}
}
func TestAssessRequiredCIOutputDoesNotAliasOrDumpSource(t *testing.T) {
	f := requiredCIFixture()
	source := fixtureJSON(t, f)
	v, err := f.assess()
	if err != nil {
		t.Fatal(err)
	}
	v.Checks[0].Context = "changed"
	v.Checks = append(v.Checks, RequiredCIAssessmentCheck{})
	if !reflect.DeepEqual(source, fixtureJSON(t, f)) {
		t.Fatal("output changed input")
	}
	wire := string(fixtureJSON(t, v))
	if strings.Contains(wire, "renamed display label") || strings.Contains(wire, "conclusion") || strings.Contains(wire, "completed_at") {
		t.Fatal("source text leaked into compact output", wire)
	}
}

func TestAssessRequiredCIPermutationsKeepCurrentRunAndExactApp(t *testing.T) {
	f := requiredCIFixture()
	old := f.Actions.Runs[0]
	old.ID, old.CheckSuiteID, old.RunNumber, old.RunAttempt = 999, 998, 11, 99
	f.Actions.Runs = append(f.Actions.Runs, old)
	c := f.Inventory.Checks[0]
	c.ID, c.SuiteID, c.Conclusion = 997, 998, "failure"
	f.Inventory.Checks = append(f.Inventory.Checks, c)
	a := f.Attempts[0]
	a.RunID, a.CheckSuiteID, a.RunNumber, a.RunAttempt = 999, 998, 11, 99
	a.Jobs = append([]ActionsJob(nil), a.Jobs...)
	a.Jobs[0].ID, a.Jobs[0].CheckRunID, a.Jobs[0].Conclusion = 996, 997, "failure"
	f.Attempts = append(f.Attempts, a)
	for mask := 0; mask < 8; mask++ {
		v := f
		v.Actions.Runs = append([]ActionsHeadRun(nil), f.Actions.Runs...)
		v.Inventory.Checks = append([]CICheckRun(nil), f.Inventory.Checks...)
		v.Attempts = append([]ActionsAttempt(nil), f.Attempts...)
		if mask&1 != 0 {
			v.Actions.Runs[0], v.Actions.Runs[1] = v.Actions.Runs[1], v.Actions.Runs[0]
		}
		if mask&2 != 0 {
			v.Inventory.Checks[0], v.Inventory.Checks[1] = v.Inventory.Checks[1], v.Inventory.Checks[0]
		}
		if mask&4 != 0 {
			v.Attempts[0], v.Attempts[1] = v.Attempts[1], v.Attempts[0]
		}
		got, err := v.assess()
		if err != nil || got.State != "satisfied" || got.Checks[0].CheckRunID != 300 || got.Checks[0].RunID != 80 || got.Checks[0].RunAttempt != 2 {
			t.Fatal(mask, got, err)
		}
	}
	// A known different app is outside a positive app requirement. It cannot
	// supply credit, or disguise the absence of the required origin.
	other := f.Inventory.Checks[0]
	other.ID, other.SuiteID, other.AppID = 991, 992, 993
	f.Inventory.Checks = append(f.Inventory.Checks, other)
	got, err := f.assess()
	if err != nil || got.State != "satisfied" {
		t.Fatal(got, err)
	}
	f.Authority.Checks[0].AppID = 0
	got, err = f.assess()
	if err != nil || got.State != "unproven" {
		t.Fatal("any-app authority hid unknown origin", got, err)
	}
}

func TestAssessRequiredCIEqualTimeStatusHistoryAndUnknownCheckDominates(t *testing.T) {
	f := requiredCIFixture()
	f.Authority.Checks[0].AppID = 0
	f.Inventory.Checks = []CICheckRun{}
	addCIStatus(&f, 999, "failure")
	addCIStatus(&f, 1, "success")
	got, err := f.assess()
	if err != nil || got.State != "failed" || got.Checks[0].StatusID != 999 {
		t.Fatal(got, err)
	}
	f.Inventory.Statuses[0], f.Inventory.Statuses[1] = f.Inventory.Statuses[1], f.Inventory.Statuses[0]
	got, err = f.assess()
	if err != nil || got.State != "satisfied" || got.Checks[0].StatusID != 1 {
		t.Fatal("did not preserve provider order", got, err)
	}
	f = requiredCIFixture()
	f.Authority.Checks[0].AppID = 0
	f.Inventory.Checks[0].SuiteID = 999
	addCIStatus(&f, 1, "failure")
	got, err = f.assess()
	if err != nil || got.State != "unproven" || got.Checks[0].StatusID != 1 {
		t.Fatal("failure concealed unproven check", got, err)
	}
}

func TestAssessRequiredCIExplicitNoRequirementsNeedsNoResultCollections(t *testing.T) {
	f := requiredCIFixture()
	f.Authority.Checks = []RequiredCheck{}
	inventory := CIResultInventory{Repository: f.Inventory.Repository, HeadSHA: f.Inventory.HeadSHA}
	// Only the authority was collected. No fabricated observed-at, empty checks,
	// empty jobs or Actions inventory is needed to report this narrow fact.
	got, err := AssessRequiredCI(f.Authority, inventory, ActionsHeadInventory{}, nil)
	if err != nil || got.State != "not_required" || got.Reason != "" || got.Repository != inventory.Repository || got.HeadSHA != inventory.HeadSHA || got.Checks == nil || len(got.Checks) != 0 {
		t.Fatalf("explicit empty authority required unrelated provider evidence: %+v %v", got, err)
	}
	for name, edit := range map[string]func(*RequiredCheckAuthority, *CIResultInventory){
		"foreignauthority":     func(a *RequiredCheckAuthority, i *CIResultInventory) { a.Repository = "foreign/repo" },
		"malformedauthority":   func(a *RequiredCheckAuthority, i *CIResultInventory) { a.Repository = "owner/repo/extra" },
		"missingauthoritytime": func(a *RequiredCheckAuthority, i *CIResultInventory) { a.ObservedAt = time.Time{} },
		"malformedbranch":      func(a *RequiredCheckAuthority, i *CIResultInventory) { a.Branch = "bad..branch" },
		"missinghead":          func(a *RequiredCheckAuthority, i *CIResultInventory) { i.HeadSHA = "" },
		"malformedhead":        func(a *RequiredCheckAuthority, i *CIResultInventory) { i.HeadSHA = strings.Repeat("A", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			a, i := f.Authority, inventory
			edit(&a, &i)
			v, err := AssessRequiredCI(a, i, ActionsHeadInventory{}, nil)
			if err == nil || v.State != "unproven" {
				t.Fatalf("invalid scope became not_required: %+v %v", v, err)
			}
		})
	}
	f.Authority.Checks = nil
	got, err = AssessRequiredCI(f.Authority, inventory, ActionsHeadInventory{}, nil)
	if err != nil || got.State != "unproven" {
		t.Fatal("missing authority treated as explicit absence", got, err)
	}
}

func TestAssessRequiredCISuccessStatusCannotReplaceSupersededWorkflowCheck(t *testing.T) {
	for _, mode := range []string{"queued", "failed", "completed_missing_check", "completed_current_check", "unrelated_workflow"} {
		t.Run(mode, func(t *testing.T) {
			f := requiredCIFixture()
			f.Authority.Checks[0].AppID = 0
			addCIStatus(&f, 9, "success")
			newest := f.Actions.Runs[0]
			newest.ID, newest.CheckSuiteID, newest.RunNumber, newest.RunAttempt = 81, 91, 13, 1
			if mode == "queued" || mode == "unrelated_workflow" {
				newest.Status, newest.Conclusion = "queued", ""
			} else if mode == "failed" {
				newest.Conclusion = "failure"
			}
			if mode == "unrelated_workflow" {
				newest.WorkflowID++
			}
			f.Actions.Runs = append(f.Actions.Runs, newest)
			if mode == "completed_current_check" {
				check := f.Inventory.Checks[0]
				check.ID, check.SuiteID = 301, newest.CheckSuiteID
				f.Inventory.Checks = append(f.Inventory.Checks, check)
				attempt := f.Attempts[0]
				attempt.RunID, attempt.CheckSuiteID, attempt.RunNumber, attempt.RunAttempt = newest.ID, newest.CheckSuiteID, newest.RunNumber, newest.RunAttempt
				job := attempt.Jobs[0]
				job.ID, job.CheckRunID = 501, check.ID
				attempt.Jobs = []ActionsJob{job}
				f.Attempts = append(f.Attempts, attempt)
			}
			before := string(fixtureJSON(t, f))
			got, err := f.assess()
			want := "unproven"
			if mode == "completed_current_check" || mode == "unrelated_workflow" {
				want = "satisfied"
			}
			if err != nil || got.State != want || got.Checks[0].StatusID != 9 || before != string(fixtureJSON(t, f)) {
				t.Fatal("legacy green status replaced a known current workflow obligation", got, err)
			}
		})
	}
}

func TestAssessRequiredCIMultipleSupersededWorkflowsDoNotMaskOneMissingCheck(t *testing.T) {
	f := requiredCIFixture()
	f.Authority.Checks[0].AppID = 0
	addCIStatus(&f, 9, "success")
	old := f.Actions.Runs[0]
	current := old
	current.ID, current.CheckSuiteID, current.RunNumber, current.RunAttempt = 81, 91, 13, 1
	otherOld, otherCurrent := old, old
	otherOld.ID, otherOld.CheckSuiteID, otherOld.WorkflowID, otherOld.RunNumber = 82, 92, 101, 1
	otherCurrent.ID, otherCurrent.CheckSuiteID, otherCurrent.WorkflowID, otherCurrent.RunNumber = 83, 93, 101, 2
	otherCurrent.RunAttempt = 1
	f.Actions.Runs = append(f.Actions.Runs, current, otherOld, otherCurrent)
	currentCheck, otherOldCheck := f.Inventory.Checks[0], f.Inventory.Checks[0]
	currentCheck.ID, currentCheck.SuiteID = 301, current.CheckSuiteID
	otherOldCheck.ID, otherOldCheck.SuiteID = 302, otherOld.CheckSuiteID
	f.Inventory.Checks = append(f.Inventory.Checks, currentCheck, otherOldCheck)
	currentAttempt := f.Attempts[0]
	currentAttempt.RunID, currentAttempt.CheckSuiteID, currentAttempt.RunNumber, currentAttempt.RunAttempt = current.ID, current.CheckSuiteID, current.RunNumber, current.RunAttempt
	job := currentAttempt.Jobs[0]
	job.ID, job.CheckRunID = 501, currentCheck.ID
	currentAttempt.Jobs = []ActionsJob{job}
	f.Attempts = append(f.Attempts, currentAttempt)
	var first RequiredCIAssessment
	for i := 0; i < 2; i++ {
		before := string(fixtureJSON(t, f))
		got, err := f.assess()
		if err != nil || got.State != "unproven" || got.Checks[0].Reason != "ci_current_check_missing" || got.Checks[0].CheckRunID != 0 || before != string(fixtureJSON(t, f)) {
			t.Fatal("one current workflow masked another missing obligation", got, err)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(first, got) {
			t.Fatal("workflow obligation depended on enumeration order", first, got)
		}
		f.Inventory.Checks[0], f.Inventory.Checks[2] = f.Inventory.Checks[2], f.Inventory.Checks[0]
		f.Actions.Runs[0], f.Actions.Runs[3] = f.Actions.Runs[3], f.Actions.Runs[0]
	}
	// Once both workflows have their own current check+job identity, the normal
	// duplicate-context rule applies; neither arbitrary workflow can win.
	otherCheck := currentCheck
	otherCheck.ID, otherCheck.SuiteID = 303, otherCurrent.CheckSuiteID
	f.Inventory.Checks = append(f.Inventory.Checks, otherCheck)
	otherAttempt := currentAttempt
	otherAttempt.RunID, otherAttempt.CheckSuiteID, otherAttempt.WorkflowID, otherAttempt.RunNumber = otherCurrent.ID, otherCurrent.CheckSuiteID, otherCurrent.WorkflowID, otherCurrent.RunNumber
	job.ID, job.CheckRunID = 502, otherCheck.ID
	otherAttempt.Jobs = []ActionsJob{job}
	f.Attempts = append(f.Attempts, otherAttempt)
	got, err := f.assess()
	if err != nil || got.State != "unproven" || got.Checks[0].Reason != "ci_check_ambiguous" {
		t.Fatal("current duplicate contexts silently selected a workflow", got, err)
	}
}

func TestRequiredCIWorkflowLifecycleMovementIsNotMalformedProviderData(t *testing.T) {
	for _, tc := range []struct{ before, after, conclusion string }{{"queued", "in_progress", ""}, {"in_progress", "completed", "success"}, {"completed", "completed", "failure"}} {
		f := requiredCIFixture()
		f.Actions.Runs[0].Status = tc.before
		if tc.before != "completed" {
			f.Actions.Runs[0].Conclusion = ""
		}
		f.Attempts[0].Status, f.Attempts[0].Conclusion = tc.after, tc.conclusion
		before := string(fixtureJSON(t, f))
		v, err := f.assess()
		if !hasCode(err, "provider_observation_changed") || v.State != "unproven" || before != string(fixtureJSON(t, f)) {
			t.Fatalf("movement credited or mislabeled: %+v %v", v, err)
		}
	}
	f := requiredCIFixture()
	f.Attempts[0].RunAttempt++
	if _, err := f.assess(); !hasCode(err, "provider_invalid_response") {
		t.Fatal("different attempt identity treated as lifecycle movement", err)
	}
}
