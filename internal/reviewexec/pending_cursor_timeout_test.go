package reviewexec

import (
	"context"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPendingCandidateTimeoutRetriesOnceThenRotates(t *testing.T) {
	e := &Engine{}
	prefix := pendingScanPosition{"2020", "prefix"}
	slow := pendingScanPosition{"2021", "slow"}
	later := pendingScanPosition{"2022", "later"}
	e.rewindTimedOutPendingCandidate(0, prefix, slow)
	if e.pendingScanCursor[0] != prefix {
		t.Fatal("late-budget candidate did not get a fresh retry")
	}
	e.rewindTimedOutPendingCandidate(1, prefix, slow)
	if e.pendingScanCursor[1] != prefix {
		t.Fatal("independent draft worker inherited ordinary worker retry")
	}
	e.rewindTimedOutPendingCandidate(0, prefix, slow)
	if e.pendingScanCursor[0] != slow || e.pendingScanTimeout[0] != (pendingScanPosition{}) {
		t.Fatal("repeated offender still blocks traversal to later candidates")
	}
	e.rewindTimedOutPendingCandidate(0, slow, later)
	if e.pendingScanCursor[0] != slow || e.pendingScanTimeout[0] != later {
		t.Fatal("a different candidate did not get its own retry")
	}
	e.finishPendingCandidateValidation(0, later)
	e.rewindTimedOutPendingCandidate(0, slow, later)
	if e.pendingScanCursor[0] != slow {
		t.Fatal("successful intervening validation did not break the streak")
	}
	e.rewindTimedOutPendingCandidate(1, prefix, slow)
	if e.pendingScanCursor[1] != slow {
		t.Fatal("draft worker did not rotate past its own repeat")
	}
}

func TestPendingCandidateTimeoutRotationReachesLaterWorkAndWraps(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	slow := f.enqueue(t, attemptProject, "slow-ordinary", "hosted", 2, 1)
	later := f.enqueue(t, otherAttemptProject, "later-ordinary", "hosted", 2, 1)
	const old = "2020-01-01T00:00:00Z"
	admissionSQL(t, f, "UPDATE review_operations SET created_at=? WHERE id=?", old, slow.ID)
	start := pendingScanPosition{}
	offender := pendingScanPosition{old, slow.ID}
	// Inject the scan's already-classified per-row deadline outcome rather
	// than racing SQLite or sleeping for the actual two-second budget.
	f.e.rewindTimedOutPendingCandidate(0, start, offender)
	f.e.rewindTimedOutPendingCandidate(0, start, offender)
	operations, err := f.e.pendingForWorker(t.Context(), false)
	if err != nil || len(operations) != 1 || operations[0].ID != later.ID {
		t.Fatal("later eligible work was starved after repeated timeout", operations, err)
	}
	if f.e.pendingScanCursor[0] != start || f.e.pendingScanTimeout[0] != start {
		t.Fatal("end of traversal did not reset to the oldest queued candidate")
	}
	operations, err = f.e.pendingForWorker(t.Context(), false)
	if err != nil || len(operations) != 2 || operations[0].ID != slow.ID {
		t.Fatal("wrapped scan dropped the retained candidate", operations, err)
	}
	if operations[0].State != "queued" || operations[0].AttemptID != "" {
		t.Fatal("rotation changed the retained request", operations[0])
	}
}

func TestMixedPendingBatchRotatesOnlyAfterRowLocalExhaustion(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	prefix := f.enqueue(t, attemptProject, "mixed-prefix-rotate", "cli", 2, 1)
	slow := f.enqueue(t, otherAttemptProject, "mixed-slow-rotate", "cli", 2, 1)
	later := f.enqueue(t, attemptProject, "mixed-later-rotate", "hosted", 2, 1)
	for i, id := range []string{prefix.ID, slow.ID, later.ID} {
		admissionSQL(t, f, "UPDATE review_operations SET created_at=? WHERE id=?", fmt.Sprintf("2020-01-01T00:00:0%dZ", i), id)
	}
	admissionSQL(t, f, "UPDATE review_execution_config SET body_json='broken' WHERE project_id=?", otherAttemptProject)
	for i := range 2 {
		scanCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		hitSlow := false
		f.e.schedulingNow = func() time.Time {
			var pcs [8]uintptr
			frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
			for {
				frame, more := frames.Next()
				if strings.HasSuffix(frame.Function, ".skipUnavailableConfiguration") {
					hitSlow = true
					<-scanCtx.Done()
					break
				}
				if !more {
					break
				}
			}
			return time.Now().UTC()
		}
		var receipt pendingExhaustion
		operations, err := f.e.scanPendingInLane(scanCtx, false, 256, 0, &receipt)
		cancel()
		f.e.schedulingNow = nil
		if err != nil || !hitSlow || len(operations) != 1 || operations[0].ID != prefix.ID || receipt.timedOut.id != slow.ID {
			t.Fatal("missing exact prefix and offender", operations, receipt, err)
		}
		// Simulate an operation read superseded before the guarded claim;
		// the durable operation and its journal remain internally consistent.
		operations[0].Revision++
		if err := f.e.processPendingBatch(t.Context(), nil, func() {}, 0, operations, receipt); err != nil {
			t.Fatal(err)
		}
		if state := supervisorOperationState(t, f, prefix.ID); state != "queued" {
			t.Fatal("stale prefix claimed", state)
		}
		if i == 0 && f.e.pendingScanCursor[0] != (pendingScanPosition{}) {
			t.Fatal("oldest prefix not replayed on first receipt")
		}
	}
	if f.e.pendingScanCursor[0].id != slow.ID {
		t.Fatal("mixed timeout did not rotate")
	}
	var chosen string
	d := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		chosen = o.ID
		return Preparation{}, core.Fail("review_evidence_pending", "fixture hold", 409)
	}}
	if err := f.e.processNextForWorker(t.Context(), map[string]Driver{"test-capability": d}, false); err != nil || chosen != later.ID {
		t.Fatal("not later unrelated work", chosen, err)
	}
	got, err := f.e.pendingForWorker(t.Context(), false)
	if err != nil || len(got) != 1 || got[0].ID != prefix.ID || got[0].AttemptID != "" {
		t.Fatal("oldest was not retained after wrap", got, err)
	}
}

func TestMixedPendingBatchUnconfirmedExitsKeepReplay(t *testing.T) {
	e := &Engine{}
	prefix := pendingScanPosition{"2020", "prefix"}
	timedOut := pendingScanPosition{"2021", "slow"}
	receipt := pendingExhaustion{prefix, timedOut}
	e.pendingScanCursor[0] = prefix
	e.pendingScanTimeout[0] = timedOut
	for _, x := range []struct{ examined, overloaded bool }{{false, false}, {true, true}, {false, true}} {
		e.finishPendingBatch(0, receipt, x.examined, x.overloaded)
		if e.pendingScanCursor[0] != prefix || e.pendingScanTimeout[0] != timedOut {
			t.Fatal("partial claim, cancel, or overload advanced replay", x)
		}
	}
}

func TestMixedPendingWorkerKeepsReplayOnOverloadCancelAndEarlyClaim(t *testing.T) {
	f := supervisorFixture(t)
	firstReceipt := f.enqueue(t, attemptProject, "mixed-control-first", "cli", 2, 1)
	tailReceipt := f.enqueue(t, attemptProject, "mixed-control-tail", "hosted", 2, 1)
	operations, err := f.e.pendingForWorker(t.Context(), false)
	if err != nil || len(operations) != 2 {
		t.Fatal(operations, err)
	}
	byID := map[string]Operation{operations[0].ID: operations[0], operations[1].ID: operations[1]}
	first, tail := byID[firstReceipt.ID], byID[tailReceipt.ID]
	if first.ID == "" || tail.ID == "" {
		t.Fatal("missing exact test operations", operations)
	}
	replay := pendingScanPosition{}
	slow := pendingScanPosition{"2020", "slow"}
	receipt := pendingExhaustion{replay, slow}
	f.e.pendingScanTimeout[0] = slow
	assertReplay := func() {
		t.Helper()
		if f.e.pendingScanCursor[0] != replay || f.e.pendingScanTimeout[0] != slow {
			t.Fatal("did not preserve prefix replay")
		}
		if state := supervisorOperationState(t, f, tail.ID); state != "queued" {
			t.Fatal("unvisited tail changed", state)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.e.processPendingBatch(cancelled, nil, func() {}, 0, []Operation{first, tail}, receipt); err != nil {
		t.Fatal(err)
	}
	assertReplay()
	db := f.store.DB()
	maxConns := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	connection, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Retain this connection for the worker's actual two-second read budget.
	err = f.e.processPendingBatch(t.Context(), nil, func() {}, 0, []Operation{first}, receipt)
	connection.Close()
	db.SetMaxOpenConns(maxConns)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay()
	var chosen string
	d := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		chosen = o.ID
		return Preparation{}, core.Fail("review_evidence_pending", "fixture hold", 409)
	}}
	if err := f.e.processPendingBatch(t.Context(), map[string]Driver{"test-capability": d}, func() {}, 0, []Operation{first, tail}, receipt); err != nil || chosen != first.ID {
		t.Fatal("wrong early claim", chosen, err)
	}
	assertReplay()
}
