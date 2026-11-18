package reviewexec

import (
	"testing"
)

func parallelCLIFixture(t *testing.T, limit int) (*attemptFixture, []Attempt) {
	t.Helper()
	f := newAttemptFixture(t)
	var err error
	if f.e, err = New(f.store, attemptCapabilities, WithCLIParallelLimit(limit)); err != nil {
		t.Fatal(err)
	}
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	first := f.start(t, attemptProject, f.enqueue(t, attemptProject, "parallel-a", "cli", 2, 1))
	second := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "parallel-b", "cli", 4, 1))
	return f, []Attempt{first, second}
}

func admitWithAllowance(t *testing.T, f *attemptFixture, a Attempt, project string, pull, remaining int64) error {
	t.Helper()
	anchor := attemptAnchor(project, pull)
	anchor.ParentSHA = anchor.BaseSHA
	if err := f.e.RecordCLIAllowance(t.Context(), project, a.ID, 1, CLIAllowanceObservation{Anchor: anchor, ObservedAt: stamp(), Remaining: remaining, Limit: 3, RollingWindowMS: 3600000, ChangedFiles: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.Admit(t.Context(), project, a.ID, a.Revision, anchor, "")
	return err
}

func TestParallelCLIRunsShareObservedAllowance(t *testing.T) {
	f, attempts := parallelCLIFixture(t, 3)
	if err := admitWithAllowance(t, f, attempts[0], attemptProject, 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := admitWithAllowance(t, f, attempts[1], otherAttemptProject, 4, 2); err != nil {
		t.Fatal("second PR blocked despite allowance", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE action_class='cli' AND execution_ended_at=''`) != 2 {
		t.Fatal("parallel runs not both live")
	}
}

func TestParallelCLIAdmissionRespectsLastAllowance(t *testing.T) {
	f, attempts := parallelCLIFixture(t, 3)
	if err := admitWithAllowance(t, f, attempts[0], attemptProject, 2, 1); err != nil {
		t.Fatal(err)
	}
	attemptCode(t, admitWithAllowance(t, f, attempts[1], otherAttemptProject, 4, 1), CLICapacityBusyReason)
	// The supervisor returns the unused preparation to its queue.
	if _, err := f.e.Advance(t.Context(), otherAttemptProject, attempts[1].ID, attempts[1].Revision, "held", CLICapacityBusyReason, ""); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE attempt_id='' AND state='queued' AND reason=?`, CLICapacityBusyReason) != 1 {
		t.Fatal("busy request not requeued")
	}
	free, err := f.e.cliAccountFree(t.Context(), f.store.DB(), "coderabbit", attempts[0].Account)
	if err != nil || free != 0 {
		t.Fatal("spent allowance offered again", free, err)
	}
}

func TestParallelCLILimitBoundsLiveRuns(t *testing.T) {
	f, attempts := parallelCLIFixture(t, 2)
	free, err := f.e.cliAccountFree(t.Context(), f.store.DB(), "coderabbit", attempts[0].Account)
	if err != nil || free != 0 {
		t.Fatal("limit not applied to live preparations", free, err)
	}
	g, _ := parallelCLIFixture(t, 3)
	if free, err = g.e.cliAccountFree(t.Context(), g.store.DB(), "coderabbit", attempts[0].Account); err != nil || free != 1 {
		t.Fatal("remaining parallel slot hidden", free, err)
	}
}
