package reviewexec

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
)

// A corrupt retained automatic revision must remain local to its project.
// The healthy project proves the supervisor does not stop at the refusal.
func TestAutomaticSupervisorIsolatesMalformedRetainedRevision(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2, 4})
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			if channel == "hosted" {
				schedulingEnable(t, f, 0, true)
				schedulingGenerate(t, f, true)
			} else {
				schedulingCLIEnable(t, f, 0, true)
				schedulingCLIGenerate(t, f, true)
			}
			selectionNext(t, f, &refreshTestReader{})
			bad := schedulingLatest(t, f).OperationID
			admissionSQL(t, f, `UPDATE review_automatic_selections SET scheduling_revision='malformed' WHERE project_id=?`, attemptProject)
			healthy := f.enqueue(t, otherAttemptProject, "healthy", channel, 2, 1)
			driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				if o.ID != healthy.ID {
					t.Error("wrong candidate reached provider", o.ID)
				}
				return Preparation{}, sourceHold("synthetic_stop")
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
				t.Fatal("local scheduling refusal stopped supervisor", err)
			}
			if driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
				t.Fatal("healthy candidate not reached", driver.prepares.Load())
			}
			if got := cliWaitRead(t, f, bad); got.State != "queued" || got.AttemptID != "" {
				t.Fatal("corrupt candidate changed", got)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE project_id=?`, attemptProject) != 0 {
				t.Fatal("corrupt project reserved attempt")
			}
		})
	}
}

func TestSelectionWorkerIsolatesMalformedRetainedRevision(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			if channel == "hosted" {
				schedulingEnable(t, f, 0, true)
				schedulingGenerate(t, f, true)
			} else {
				schedulingCLIEnable(t, f, 0, true)
				schedulingCLIGenerate(t, f, true)
			}
			bad := schedulingLatest(t, f)
			admissionSQL(t, f, `UPDATE review_automatic_selections SET scheduling_revision='malformed' WHERE project_id=?`, attemptProject)
			beforeFrames := snapshotTable(t, f, "review_front_frames")
			worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
			if err != nil || !worked {
				t.Fatal("local failure stopped worker", worked, err)
			}
			got := selectionRead(t, f, bad.ID)
			if got.State != "held" || got.Reason != "automatic_scheduling_unavailable" || got.OperationID != "" {
				t.Fatal("incorrect refusal attribution", got)
			}
			if beforeFrames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
				t.Fatal("partial source or child retained")
			}
			raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "healthy", RunRequest{Channel: channel})
			if err != nil {
				t.Fatal(err)
			}
			var healthy Receipt
			if err = json.Unmarshal(raw, &healthy); err != nil {
				t.Fatal(err)
			}
			readers := map[string]TargetReader{"test-capability": &refreshTestReader{}, "other-account": &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				return reviewprovider.Pull{Number: n, RepositoryID: 802, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}}
			if worked, err = f.e.runNext(t.Context(), readers); err != nil || !worked {
				t.Fatal("healthy project stopped", worked, err)
			}
			var state string
			if err = f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, healthy.ID).Scan(&state); err != nil || state != "selected" {
				t.Fatal("healthy project not selected", state, err)
			}
			if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got = selectionRead(t, f, bad.ID); got.State != "held" {
				t.Fatal("restart reopened poison selection", got)
			}
		})
	}
}

// A revision can become unusable after initial claim while provider reads run.
// The failed finish must not leave front/source writes or a child behind.
func TestSelectionLocalRefusalAfterSourceReadRollsBack(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	r := schedulingLatest(t, f)
	before := snapshotTable(t, f, "review_front_frames")
	reader := &refreshTestReader{repository: func(_ context.Context, _ int64) (reviewprovider.Repository, error) {
		admissionSQL(t, f, `UPDATE review_automatic_selections SET scheduling_revision='malformed' WHERE selection_id=?`, r.ID)
		return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}}
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	got := selectionRead(t, f, r.ID)
	if got.State != "held" || got.Reason != "automatic_scheduling_unavailable" || got.OperationID != "" {
		t.Fatal(got)
	}
	if before != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("partial source or child survived refusal")
	}
}
