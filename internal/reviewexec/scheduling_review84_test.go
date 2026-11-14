package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

func TestReview84DraftWaitDoesNotFenceIndependentCLI(t *testing.T) {
	f := schedulingFixture(t)
	op := f.enqueue(t, attemptProject, "draft", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE review_operations SET reason='cli_draft_wait' WHERE id=?`, op.ID)
	err := f.store.Read(t.Context(), f.scope(t, attemptProject, f.admin, true), func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		limit, err := automaticHostedQueuedPrefixChannel(ctx, tx, attemptProject, c, "cli")
		if err == nil && limit != 2 {
			t.Fatalf("draft fenced independent successor: %d", limit)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := roundOperation(t, f, op.ID); got.State != "queued" || got.Reason != cliDraftWait {
		t.Fatal(got)
	}
}

func TestReview84MalformedPreferredWaitIsolation(t *testing.T) {
	for _, account := range []string{"test-capability", "other-account"} {
		t.Run(account, func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, account, 0, false)
			low := f.enqueue(t, attemptProject, "healthy", "cli", 2, 1)
			high := f.enqueue(t, otherAttemptProject, "broken", "cli", 2, 1)
			admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, otherAttemptProject)
			aHigh := f.start(t, otherAttemptProject, high)
			admissionSQL(t, f, `UPDATE review_operations SET state='queued',attempt_id='' WHERE id=?`, high.ID)
			admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, aHigh.ID)
			admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,?,'malformed',0)`, high.ID, aHigh.ID)
			a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID)
			if account == "test-capability" {
				if !isWaitUnavailable(err) || a.ID != "" {
					t.Fatal("shared-account corrupt cooldown must remain fail-closed", a, err)
				}
			} else if err != nil || a.ID == "" {
				t.Fatal("damaged wait blocked independent account", a, err)
			}
		})
	}
}

func TestReview84AutomaticCLIIndependentAccountsBothProgress(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, attemptProject)
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-other-cred'`)
	schedulingCLIEnable(t, f, 0, true)
	zero := int64(0)
	yes := true
	no := false
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, otherAttemptProject, f.other, "write"), "enable-other", SchedulingRequest{ExpectedRevision: &zero, AutomaticCLI: &yes, AutomaticHosted: &no})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := f.e.generateAutomaticCLI(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}, "other-account": &refreshTestReader{}})
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	for _, project := range []string{attemptProject, otherAttemptProject} {
		var n int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_automatic_selections WHERE project_id=?`, project).Scan(&n); err != nil || n != 1 {
			t.Fatalf("project %s starved: %d %v", project, n, err)
		}
	}
}
