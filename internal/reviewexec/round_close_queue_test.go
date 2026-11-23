package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func TestRoundCloseWithdrawsPausedHostedCooldownAtomically(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 0, 3, "grant-hosted")
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "bound-cooldown", "hosted", 2, 1))
	retainTimerCooldown(t, f, a, 3*time.Minute)
	if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	timerControl(t, f, a.OperationID, "pause", "pause-held")
	paused := roundOperation(t, f, a.OperationID)
	if paused.State != "queued" || paused.AttemptID != "" || paused.Wait == nil || !paused.Wait.Paused || paused.Wait.BaselineAttemptID != a.ID || paused.Wait.RetryNotBefore == "" {
		t.Fatal("fixture has no paused queued cooldown", paused)
	}
	cli := f.enqueue(t, attemptProject, "other-channel", "cli", 2, 1)
	otherPull := f.enqueue(t, attemptProject, "other-pull", "hosted", 4, 1)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	otherProject := f.enqueue(t, otherAttemptProject, "other-project", "hosted", 2, 1)
	retained := func() string {
		return roundTables(t, f) + snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") +
			snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_execution_reservations")
	}
	journal := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_execution_reservations")
	before := retained()
	scope := f.scope(t, attemptProject, f.writer, false)
	revision := int64(1)
	input := RoundControlRequest{ExpectedRevision: &revision, Action: "close"}
	admissionSQL(t, f, `CREATE TRIGGER fail_closure_cancel_event BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'injected closure cancellation event failure'); END`)
	if _, err := f.e.ConfigureRoundControl(t.Context(), scope, "close-lane", 2, "hosted", input); err == nil || retained() != before {
		t.Fatal("failed cancellation event partially applied closure, withdrawal, or receipt", err)
	}
	admissionSQL(t, f, `DROP TRIGGER fail_closure_cancel_event`)
	first, err := f.e.ConfigureRoundControl(t.Context(), scope, "close-lane", 2, "hosted", input)
	if err != nil {
		t.Fatal(err)
	}
	closed := roundRead(t, f, 2, "hosted")
	if !closed.Closed || closed.Revision != 2 || closed.Used != 0 || closed.Dry != 0 || roundPendingCount(t, f, 2, "hosted") != 0 {
		t.Fatal("closed lane kept pending work or earned credit", closed)
	}
	withdrawn := roundOperation(t, f, a.OperationID)
	if withdrawn.State != "cancelled" || withdrawn.Reason != "review_round_closed" || withdrawn.Revision != paused.Revision+1 || withdrawn.AttemptID != "" || withdrawn.Wait == nil || withdrawn.Wait.Paused ||
		withdrawn.Wait.BaselineAttemptID != a.ID || withdrawn.Wait.RetryNotBefore != paused.Wait.RetryNotBefore || currentOperationWait(withdrawn) != nil {
		t.Fatal("cancellation lost audit evidence or retained a live wait", withdrawn)
	}
	if journal != snapshotTable(t, f, "review_execution_attempts")+snapshotTable(t, f, "review_execution_transitions")+snapshotTable(t, f, "review_execution_reservations") {
		t.Fatal("closure changed attempt history or reservations")
	}
	for _, untouched := range []Receipt{cli, otherPull, otherProject} {
		if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE id=? AND state='queued' AND revision=?`, untouched.ID, untouched.Revision) != 1 {
			t.Fatal("closure changed work outside its project, pull, or channel", untouched.ID)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE project_id=? AND event_type='review.operation_updated' AND resource_id=? AND revision=? AND actor_id=?`, scope.ProjectID, withdrawn.ID, withdrawn.Revision, scope.PrincipalID) != 1 ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes WHERE project_id=? AND pull_number=2 AND channel='hosted' AND revision=2 AND worker_id=?`, scope.ProjectID, scope.WorkerID) != 1 {
		t.Fatal("closure did not emit an attributed operation event")
	}
	before = retained()
	replay, err := f.e.ConfigureRoundControl(t.Context(), scope, "close-lane", 2, "hosted", input)
	if err != nil || !bytes.Equal(replay, first) || retained() != before {
		t.Fatal("exact closure replay repeated mutations", err)
	}
	revision = 2
	if _, err := f.e.ConfigureRoundControl(t.Context(), scope, "fresh-close", 2, "hosted", input); err != nil || !reflect.DeepEqual(withdrawn, roundOperation(t, f, withdrawn.ID)) {
		t.Fatal("fresh close of closed lane changed terminal operation history", err)
	}
	roundSnapshotValid(t, f)
}

func TestRoundCloseWithdrawsPreviouslyClosedRetainedQueue(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 0, 3, "grant-hosted")
	request := f.enqueue(t, attemptProject, "retained-request", "hosted", 2, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	// Reproduce a valid pre-fix closure with an untouched queued operation.
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		control, err := readRoundRecord(ctx, tx, scope.ProjectID, 2, "hosted")
		if err != nil {
			return err
		}
		control.Closed = true
		control.Revision++
		control.ActorID, control.UpdatedAt = scope.PrincipalID, stamp()
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_round_changes(project_id,pull_number,channel,revision,epoch,action,body_json,worker_id) VALUES(?,?,?,?,?,'close',?,?)`, control.ProjectID, control.PullNumber, control.Channel, control.Revision, control.Epoch, encodeRoundHistory(control.RoundControl, control.cumulative), scope.WorkerID); err != nil {
			return err
		}
		return storeRoundRecord(ctx, tx, control)
	})
	roundSnapshotValid(t, f)
	old := roundRead(t, f, 2, "hosted")
	if !old.Closed || roundOperation(t, f, request.ID).State != "queued" {
		t.Fatal("fixture did not retain an undispatched request after closure")
	}
	frame, err := f.e.readRoundProgress(t.Context(), scope, []int64{2}, roundProgressView{stages: true, sourceRetry: true})
	if err != nil {
		t.Fatal(err)
	}
	progress := frame.Items[0].Channels[0]
	if !progress.Closed || progress.Pending != 0 || progress.Wait != nil || progress.SourceRetry != nil {
		t.Fatal("closed lane projected an undispatched request as current work", progress)
	}
	if _, err := f.e.ConfigureRoundControl(t.Context(), scope, "close-retained-lane", 2, "hosted", RoundControlRequest{ExpectedRevision: &old.Revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	current, operation := roundRead(t, f, 2, "hosted"), roundOperation(t, f, request.ID)
	if !current.Closed || current.Revision != old.Revision+1 || current.Epoch != old.Epoch || current.Used != old.Used || operation.State != "cancelled" || operation.AttemptID != "" {
		t.Fatal("fresh close did not reconcile the retained queue without changing credit", current, operation)
	}
	roundSnapshotValid(t, f)
}
