package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func TestAutomaticStoppedPreparationRetryBothChannels(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			enable, generate := schedulingEnable, schedulingGenerate
			if channel == "cli" {
				enable, generate = schedulingCLIEnable, schedulingCLIGenerate
			}
			enable(t, f, 0, true)
			generate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			run := schedulingLatest(t, f)
			o := cliWaitRead(t, f, run.OperationID)
			a := f.start(t, attemptProject, Receipt{ID: o.ID})
			stopped, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "cancelled", "supervisor_stopping", "")
			if err != nil {
				t.Fatal(err)
			}
			terminal := cliWaitRead(t, f, o.ID)
			at, _ := attemptRecordedTime(terminal.UpdatedAt)
			now := at.Add(transientSourceDelay - time.Nanosecond)
			f.e.schedulingNow = func() time.Time { return now }
			generate(t, f, false)
			now = at.Add(transientSourceDelay)
			generate(t, f, true)
			next := cliWaitRead(t, f, o.ID)
			if next.State != "queued" || next.AttemptID != "" || next.CreatedAt != o.CreatedAt || next.Revision != terminal.Revision+1 {
				t.Fatal(next)
			}
			if attemptCount(t, f.store, "SELECT count(*) FROM review_operations") != 1 || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_attempts") != 1 {
				t.Fatal("duplicated operation or attempt")
			}
			old, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID)
			if err != nil || !reflect.DeepEqual(old, stopped) {
				t.Fatal("history changed", err)
			}
			if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateOperationWaitsSnapshot(ctx, tx) }); err != nil {
				t.Fatal("backup history invalid", err)
			}
			second := f.start(t, attemptProject, Receipt{ID: o.ID})
			if _, err = f.e.Advance(t.Context(), attemptProject, second.ID, second.Revision, "cancelled", "supervisor_stopping", ""); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			generate(t, f, false)
		})
	}
}

func TestAutomaticStoppedPreparationDoesNotRetryOtherCancellation(t *testing.T) {
	for _, reason := range []string{"operator_verified_external_stop", "source_superseded"} {
		t.Run(reason, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			o := cliWaitRead(t, f, schedulingLatest(t, f).OperationID)
			a := f.start(t, attemptProject, Receipt{ID: o.ID})
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "cancelled", reason, ""); err != nil {
				t.Fatal(err)
			}
			f.e.schedulingNow = func() time.Time { return time.Now().Add(time.Hour) }
			schedulingGenerate(t, f, false)
		})
	}
}

func TestAutomaticStoppedPreparationPreservesQueuedOperatorCancel(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	o := cliWaitRead(t, f, schedulingLatest(t, f).OperationID)
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "explicit-cancel", o.ID, OperationControl{ExpectedRevision: o.Revision, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	f.e.schedulingNow = func() time.Time { return time.Now().Add(time.Hour) }
	schedulingGenerate(t, f, false)
	final := cliWaitRead(t, f, o.ID)
	if final.State != "cancelled" || final.AttemptID != "" {
		t.Fatal(final)
	}
}

func TestAutomaticStoppedPreparationRejectsRetainedProviderArtifacts(t *testing.T) {
	for _, table := range []string{"review_cli_preflight", "review_hosted_preflight"} {
		t.Run(table, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			o := cliWaitRead(t, f, schedulingLatest(t, f).OperationID)
			a := f.start(t, attemptProject, Receipt{ID: o.ID})
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "cancelled", "supervisor_stopping", ""); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, "INSERT INTO "+table+"(attempt_id,project_id,body_json) VALUES(?,?,?)", a.ID, attemptProject, "{}")
			f.e.schedulingNow = func() time.Time { return time.Now().Add(time.Hour) }
			schedulingGenerate(t, f, false)
		})
	}
}
