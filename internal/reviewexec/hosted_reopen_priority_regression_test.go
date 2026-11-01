package reviewexec

import (
	"reflect"
	"testing"
	"time"
)

func TestHostedAutomaticReopenedEarlierPRPrecedesRetainedLaterCooldown(t *testing.T) {
	f := schedulingFixture(t)
	zero := int64(0)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-earlier", 2, "hosted", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	later := schedulingLatest(t, f)
	scope := selectionScope(t, f, attemptProject, f.writer, "read")
	op, err := f.e.ReadOperation(t.Context(), scope, later.OperationID)
	if err != nil || later.State != "selected" || op.PullNumber != 4 {
		t.Fatal("later PR was not selected after earlier closure", later, op, err)
	}
	due := priorityCooldown(t, f, Receipt{ID: op.ID}, 4, 3*time.Minute)
	retained, err := f.e.ReadOperation(t.Context(), scope, op.ID)
	if err != nil || retained.Wait == nil {
		t.Fatal("missing retained cooldown", retained, err)
	}
	// An unchanged tick does not prefetch another round or another successor.
	schedulingGenerate(t, f, false)
	roundGrant(t, f, 2, "hosted", 1, 1, 2, "reopen-earlier")
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	earlier := schedulingLatest(t, f)
	head, err := f.e.ReadOperation(t.Context(), scope, earlier.OperationID)
	if err != nil || earlier.State != "selected" || head.PullNumber != 2 || earlier.OperationID == later.OperationID {
		t.Fatal("reopened earlier PR had no distinct prioritized intent", earlier, head, err)
	}
	unchanged, err := f.e.ReadOperation(t.Context(), scope, later.OperationID)
	if err != nil || !reflect.DeepEqual(unchanged, retained) {
		t.Fatal("reconsideration cancelled or rewrote later cooldown", unchanged, retained, err)
	}
	// A grant changes the semantic basis, but cannot create a second pending
	// request on a PR that already has one.
	roundGrant(t, f, 2, "hosted", 2, 0, 1, "more-while-pending")
	schedulingGenerate(t, f, false)
	if got := attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE project_id='exec-project-one' AND channel='hosted'`); got != 2 {
		t.Fatal("duplicate automatic request", got)
	}
	f.e.schedulingNow = func() time.Time { return due }
	priorityPending(t, f, head.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, retained.ID); err != nil || a.ID != "" {
		t.Fatal("older later request passed the reopened front", a, err)
	}
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, head.ID); err != nil || a.ID == "" {
		t.Fatal("earlier request could not claim normally", a, err)
	}
	if roundRead(t, f, 2, "hosted").Used != 0 || roundRead(t, f, 4, "hosted").Used != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("planning or claiming manufactured review credit")
	}
}

func TestHostedAutomaticReconsiderationPreservesExplicitAndActiveWork(t *testing.T) {
	t.Run("queued-front-does-not-prefetch", func(t *testing.T) {
		f := schedulingFixture(t)
		f.enqueue(t, attemptProject, "explicit-front", "hosted", 2, 1)
		schedulingEnable(t, f, 0, true)
		before := selectionCounts(t, f)
		schedulingGenerate(t, f, false)
		if after := selectionCounts(t, f); after != before {
			t.Fatal("waiting front caused extra automatic work", before, after)
		}
	})
	t.Run("active-work-not-interrupted", func(t *testing.T) {
		f := schedulingFixture(t)
		later := f.enqueue(t, attemptProject, "already-preparing", "hosted", 4, 1)
		f.start(t, attemptProject, later)
		schedulingEnable(t, f, 0, true)
		before := selectionCounts(t, f)
		schedulingGenerate(t, f, false)
		if after := selectionCounts(t, f); after != before {
			t.Fatal("active work caused automatic replacement", before, after)
		}
	})
	t.Run("explicit-selection-still-refuses-inflight", func(t *testing.T) {
		f := schedulingFixture(t)
		f.enqueue(t, attemptProject, "explicit-later", "hosted", 4, 1)
		r, _ := selectionEnqueue(t, f, "explicit-selection", "hosted", nil)
		reader := &refreshTestReader{}
		selectionNext(t, f, reader)
		selected := selectionRead(t, f, r.ID)
		if selected.State != "held" || selected.Reason != "review_inflight" || reader.calls.Load() != 0 {
			t.Fatal("automatic exception changed explicit selection", selected, reader.calls.Load())
		}
	})
}
