package reviewexec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestReview636AncestorHoldBeforeAllChildAdmission(t *testing.T) {
	for _, mode := range []string{"covered", "closed", "tapered", "merged"} {
		t.Run(mode, func(t *testing.T) {
			f, prior, id, predecessor := coverageFixture(t, 0, false)
			coverageFinalize(t, f, prior, id, 0)
			if mode == "closed" {
				zero := int64(0)
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			} else if mode == "tapered" {
				if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'adoption-worker','Synthetic importer')`, attemptProject); err != nil {
					t.Fatal(err)
				}
				p := adoptionPrepared()
				p.Tapered, p.InitialDry = true, 3
				adoptRound(t, f, p, "taper")
			} else if mode == "merged" {
				predecessor.Pull.State, predecessor.Pull.Merged = "closed", true
			}
			previous := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
				t.Fatal(err)
			}
			_, _, hold := integrationImportHold(t, f, 2)
			beforeHold := integrationHoldDetail(t, f, hold.ID)
			operation := f.enqueue(t, attemptProject, "child", "cli", 4, 2)
			child := f.start(t, attemptProject, operation)
			if _, err := f.e.ExecutionContext(t.Context(), attemptProject, operation.ID); err != nil {
				t.Fatal("preparation blocked by ancestor", err)
			}
			if _, err := f.e.RoundAdmission(t.Context(), attemptProject, operation.ID); err != nil {
				t.Fatal("round admission blocked by ancestor", err)
			}
			anchor := attemptAnchor(attemptProject, 4)
			anchor.ParentSHA = anchor.BaseSHA
			var predecessors []SourcePlan
			if mode != "merged" {
				predecessor.Pull.HeadRef = "actual-parent"
				anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, predecessor.Pull.HeadSHA, predecessor.Pull.HeadSHA, "actual-parent"
				predecessors = []SourcePlan{predecessor}
			}
			var err error
			child, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, predecessors...); err != nil {
				t.Fatal("baseline predecessor failed", err)
			}
			before := attemptState(t, f, child)

			if mode == "merged" {
				// A merged former queue prefix is not an ancestor of a main-based
				// target and cannot be supplied as its dependency witness.
				attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, predecessor), "validation_failed")
				if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision); err != nil {
					t.Fatal("unrelated merged hold blocked independent target", err)
				}
				_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "merged-source-still-held", Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
				integrationSourceHeld(t, err)
			} else {
				if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, predecessors...); err != nil {
					t.Fatal("ancestor adoption work blocked child review", err)
				}
				_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "ancestor-target-still-held", Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
				integrationSourceHeld(t, err)
			}
			if after := integrationHoldDetail(t, f, hold.ID); !reflect.DeepEqual(beforeHold, after) || after.Status != "open" {
				t.Fatal("final guard changed the retained source hold", beforeHold, after)
			}
			if attemptState(t, f, child) != before {
				t.Fatal("predecessor hold rewrote child work")
			}
		})
	}
}

func TestReview636CompletedSourceRetryUsesCurrentSharedRevision(t *testing.T) {
	f, stale := sourceRetryFixture(t)
	ageSourceRetry(t, f, stale.ID)
	stale = selectionRead(t, f, stale.ID)
	one := int64(1)
	on := true
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "other-channel", SchedulingRequest{ExpectedRevision: &one, AutomaticHosted: &on, AutomaticCLI: &on})
	if err != nil {
		t.Fatal(err)
	}
	assertRevision := func(selectionID string, acceptedWant, currentWant int64) {
		t.Helper()
		var accepted, current int64
		err := f.store.DB().QueryRow(`SELECT a.scheduling_revision,s.revision FROM review_automatic_selections a
			JOIN review_scheduling s ON s.project_id=a.project_id WHERE a.selection_id=? AND a.project_id=?`,
			selectionID, attemptProject).Scan(&accepted, &current)
		if err != nil || accepted != acceptedWant || current != currentWant {
			t.Fatalf("retained selection revision=%d current=%d, want %d/%d: %v", accepted, current, acceptedWant, currentWant, err)
		}
	}
	assertRevision(stale.ID, 1, 2)

	// Changing only CLI leaves the Hosted intent basis unchanged, but must not
	// requeue a completed hold under its superseded shared authorization.
	reader := &refreshTestReader{}
	worked, err := f.e.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || worked || reader.calls.Load() != 0 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatalf("stale retry performed work or replaced the held intent: worked=%t calls=%d err=%v", worked, reader.calls.Load(), err)
	}
	if got := selectionRead(t, f, stale.ID); got != stale {
		t.Fatalf("stale held selection was rewritten: %+v", got)
	}

	// A deliberate Hosted rearm creates a distinct current-authority intent.
	// When that fresh intent reaches a transient hold, its bounded retry must
	// still work; the previous settings edit isn't a permanent veto.
	schedulingEnable(t, f, 2, true)
	schedulingGenerate(t, f, true)
	fresh := schedulingLatest(t, f)
	if fresh.ID == stale.ID || fresh.State != "queued" {
		t.Fatalf("fresh revision did not obtain its own intent: %+v", fresh)
	}
	assertRevision(fresh.ID, 3, 3)
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	if got := selectionRead(t, f, fresh.ID); got.State != "held" || got.Reason != "source_fetch_timeout" || got.OperationID != "" {
		t.Fatalf("fresh intent did not retain a transient hold: %+v", got)
	}
	ageSourceRetry(t, f, fresh.ID)
	schedulingGenerate(t, f, true)
	if got := selectionRead(t, f, fresh.ID); got.State != "queued" || got.OperationID != "" {
		t.Fatalf("current held intent did not requeue: %+v", got)
	}
	reader = &refreshTestReader{}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, fresh.ID)
	if got.State != "selected" || got.OperationID == "" || reader.calls.Load() == 0 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 2 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatalf("current retry did not create exactly one child without provider execution: %+v", got)
	}
	if old := selectionRead(t, f, stale.ID); old != stale {
		t.Fatalf("fresh retry rewrote the old held intent: %+v", old)
	}
}
