package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func latestActionsFixture() ActionsHeadInventory {
	return ActionsHeadInventory{Repository: "owner/repo", RepositoryID: 20, HeadSHA: headRunsSHA, ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 123, time.UTC), Runs: []ActionsHeadRun{}}
}

func latestActionsRun(id, workflow, number int64) ActionsHeadRun {
	return ActionsHeadRun{ID: id, CheckSuiteID: id + 1000, WorkflowID: workflow, RunNumber: number, RunAttempt: 1, RepositoryID: 20, Repository: "Owner/Repo", HeadSHA: headRunsSHA, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: time.Date(2026, 1, 1, 1, 2, 3, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 1, 2, 4, 0, time.UTC)}
}

func TestLatestActionsRunsSelectsPerWorkflowProviderNumberOnly(t *testing.T) {
	in := latestActionsFixture()
	oldGreen := latestActionsRun(900, 30, 15)
	oldGreen.RunAttempt = 99 // A partial rerun of an older run is still older.
	oldGreen.UpdatedAt = in.ObservedAt
	newPending := latestActionsRun(2, 30, 16)
	newPending.Status, newPending.Conclusion = "queued", ""
	// An earlier timestamp, lower ID and different suite cannot hide the new run.
	newPending.CreatedAt = oldGreen.CreatedAt.Add(-time.Hour)
	newPending.UpdatedAt = newPending.CreatedAt
	otherWorkflow := latestActionsRun(3, 10, 1)
	otherWorkflow.Event, otherWorkflow.Conclusion, otherWorkflow.RunAttempt = "workflow_dispatch", "failure", 3
	olderOther := latestActionsRun(4, 10, 0)
	olderOther.RunNumber = 1
	otherWorkflow.RunNumber = 2
	in.Runs = []ActionsHeadRun{oldGreen, newPending, otherWorkflow, olderOther}
	want := in
	want.Runs = []ActionsHeadRun{otherWorkflow, newPending}
	// All orderings must preserve exact selected facts and stable workflow order.
	var permutations func(int)
	permutations = func(index int) {
		if index == len(in.Runs) {
			before := append([]ActionsHeadRun(nil), in.Runs...)
			got, err := LatestActionsRuns(in)
			if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(in.Runs, before) {
				t.Fatal("selection filtered/reordered source or guessed currentness", got, err)
			}
			return
		}
		for i := index; i < len(in.Runs); i++ {
			in.Runs[index], in.Runs[i] = in.Runs[i], in.Runs[index]
			permutations(index + 1)
			in.Runs[index], in.Runs[i] = in.Runs[i], in.Runs[index]
		}
	}
	permutations(0)
}

func TestLatestActionsRunsRetainsEverySelectedLifecycleAndOwnsSlice(t *testing.T) {
	in := latestActionsFixture()
	for i, state := range []string{"queued", "in_progress", "waiting", "pending", "requested", "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure"} {
		run := latestActionsRun(int64(i+1), int64(i+1), 1)
		switch state {
		case "queued", "in_progress", "waiting", "pending", "requested":
			run.Status, run.Conclusion = state, ""
		default:
			run.Conclusion = state
		}
		in.Runs = append(in.Runs, run)
	}
	got, err := LatestActionsRuns(in)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatal("selected lifecycle facts were filtered", got, err)
	}
	got.Runs[0].Event = "changed in returned value"
	if in.Runs[0].Event != "pull_request" {
		t.Fatal("output aliases input")
	}
	in.Runs[1].Status = "changed in input"
	if got.Runs[1].Status != "in_progress" {
		t.Fatal("input aliases output")
	}
	empty, err := LatestActionsRuns(latestActionsFixture())
	if err != nil || empty.Runs == nil || len(empty.Runs) != 0 {
		t.Fatal("proven empty inventory changed", empty, err)
	}
}

func TestLatestActionsRunsRejectsMalformedInventoriesWithoutPartialSelection(t *testing.T) {
	cases := map[string]func(*ActionsHeadInventory){
		"nil rows":                    func(v *ActionsHeadInventory) { v.Runs = nil },
		"bad selector":                func(v *ActionsHeadInventory) { v.Repository = "owner/repo/extra" },
		"missing repository identity": func(v *ActionsHeadInventory) { v.RepositoryID = 0 },
		"uppercase selected head":     func(v *ActionsHeadInventory) { v.HeadSHA = strings.ToUpper(v.HeadSHA) },
		"missing observed time":       func(v *ActionsHeadInventory) { v.ObservedAt = time.Time{} },
		"invalid observed year":       func(v *ActionsHeadInventory) { v.ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"zero run":                    func(v *ActionsHeadInventory) { v.Runs[0].ID = 0 },
		"zero suite":                  func(v *ActionsHeadInventory) { v.Runs[0].CheckSuiteID = 0 },
		"zero workflow":               func(v *ActionsHeadInventory) { v.Runs[0].WorkflowID = 0 },
		"zero number":                 func(v *ActionsHeadInventory) { v.Runs[0].RunNumber = 0 },
		"zero attempt":                func(v *ActionsHeadInventory) { v.Runs[0].RunAttempt = 0 },
		"wrong repository id":         func(v *ActionsHeadInventory) { v.Runs[0].RepositoryID++ },
		"wrong repository name":       func(v *ActionsHeadInventory) { v.Runs[0].Repository = "other/repo" },
		"wrong head":                  func(v *ActionsHeadInventory) { v.Runs[0].HeadSHA = strings.Repeat("b", 40) },
		"bad head":                    func(v *ActionsHeadInventory) { v.Runs[0].HeadSHA = "abc" },
		"empty event":                 func(v *ActionsHeadInventory) { v.Runs[0].Event = "" },
		"unsafe event":                func(v *ActionsHeadInventory) { v.Runs[0].Event = "push\n" },
		"unknown status":              func(v *ActionsHeadInventory) { v.Runs[0].Status = "future" },
		"unknown conclusion":          func(v *ActionsHeadInventory) { v.Runs[0].Conclusion = "passed" },
		"missing completion":          func(v *ActionsHeadInventory) { v.Runs[0].Conclusion = "" },
		"pending conclusion":          func(v *ActionsHeadInventory) { v.Runs[0].Status = "queued" },
		"zero created":                func(v *ActionsHeadInventory) { v.Runs[0].CreatedAt = time.Time{} },
		"zero updated":                func(v *ActionsHeadInventory) { v.Runs[0].UpdatedAt = time.Time{} },
		"reversed times":              func(v *ActionsHeadInventory) { v.Runs[0].UpdatedAt = v.Runs[0].CreatedAt.Add(-time.Second) },
		"duplicate exact id":          func(v *ActionsHeadInventory) { v.Runs = append(v.Runs, v.Runs[0]) },
		"conflicting id": func(v *ActionsHeadInventory) {
			r := v.Runs[0]
			r.WorkflowID++
			r.RunNumber++
			r.CheckSuiteID++
			v.Runs = append(v.Runs, r)
		},
		"duplicate workflow number": func(v *ActionsHeadInventory) {
			r := v.Runs[0]
			r.ID++
			r.CheckSuiteID++
			r.RunAttempt++
			v.Runs = append(v.Runs, r)
		},
		"same suite distinct run": func(v *ActionsHeadInventory) { r := v.Runs[0]; r.ID++; r.WorkflowID++; v.Runs = append(v.Runs, r) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := latestActionsFixture()
			in.Runs = []ActionsHeadRun{latestActionsRun(1, 1, 1)}
			mutate(&in)
			got, err := LatestActionsRuns(in)
			if err == nil || !reflect.DeepEqual(got, ActionsHeadInventory{}) {
				t.Fatal("malformed facts yielded a partial selection", got, err)
			}
		})
	}
}

func TestLatestActionsRunsPreservesFilteredInventoryBoundary(t *testing.T) {
	in := latestActionsFixture()
	for i := int64(1); i <= 1000; i++ {
		in.Runs = append(in.Runs, latestActionsRun(i, i, i))
	}
	got, err := LatestActionsRuns(in)
	if err == nil || !reflect.DeepEqual(got, ActionsHeadInventory{}) {
		t.Fatal("filtered search truncation boundary became a selection", got, err)
	}
	in.Runs = in.Runs[:999]
	got, err = LatestActionsRuns(in)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatal("bounded complete inventory rejected or changed", err)
	}
}
