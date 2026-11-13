package reviewexec

import (
	"fmt"
	"testing"
	"time"
)

func TestRetainedCorePauseBetweenStartAndAdmit(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	revision := int64(1)
	c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "pause-after-start", c); err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	_, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, in.Anchor, "")
	attemptCode(t, err, "review_conflict")
	if before != attemptState(t, f, a) {
		t.Fatal("refused admission changed attempt")
	}
}

func TestRetainedCoreLateCLIRecoveryRearmsFromTransition(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	*now = now.Add(24 * time.Hour)
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	due := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
	if due.Sub(*now) != time.Minute {
		t.Fatal("not rearmed from recovery", due, *now)
	}
	assertCLIWaitSnapshot(t, f)
}

func TestRetainedCorePositiveAllowanceDraftHistory(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", cliDraftWait, ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
		t.Fatal(err)
	}
	// The permit is history, not reusable execution authority.
	o := cliWaitRead(t, f, a.OperationID)
	if o.State != "queued" || o.AttemptID != "" || o.Reason != cliDraftWait {
		t.Fatal(o)
	}
	admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json='{}' WHERE attempt_id=?`, a.ID)
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err == nil {
		t.Fatal("corrupt retained permit accepted")
	}
}

func TestRetainedCoreMissingLiveCLIWaitRefusesAccount(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, a.OperationID)
	_, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities["test-capability"], *now)
	if !isStoredUnavailable(err, storedWaitingIntent) {
		t.Fatal("missing live timer did not refuse account", err)
	}
	// Expired retained damage is not a live account timer.
	if _, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities["test-capability"], now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedCoreLongCompatibleHistoryAdmission(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	// Populate a valid alternating suffix history in one transaction, avoiding
	// thousands of unrelated administrative events in this latency probe.
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for revision := int64(2); revision <= 5001; revision++ {
		suffix := int64(4)
		if revision%2 == 0 {
			suffix = 6
		}
		expected := revision - 1
		c := Configuration{ExpectedRevision: &expected, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, suffix}, Channels: []string{"cli", "hosted"}}
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, attemptProject, revision, encoded(c)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err = f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID); err != nil {
		t.Fatal("bounded context failed", err)
	}
	t.Log("ExecutionContext 5000 suffix revisions", time.Since(start))
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, in.Anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err = f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal("bounded final guard failed", err)
	}
	t.Log("VerifyAction 5000 suffix revisions", time.Since(start))
}

func TestRetainedCoreInterveningParentRemoval(t *testing.T) {
	for _, afterAdmit := range []bool{false, true} {
		t.Run(fmt.Sprint(afterAdmit), func(t *testing.T) {
			f, prior, id, parent := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "reject-parent", 0, 0, "rejected")
			coverageFinalize(t, f, prior, id, 1)
			revision := int64(1)
			c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{4, 2, 6}, Channels: []string{"cli", "hosted"}}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", c); err != nil {
				t.Fatal(err)
			}
			child := f.start(t, attemptProject, f.enqueue(t, attemptProject, "child", "cli", 4, 2))
			anchor := attemptAnchor(attemptProject, 4)
			anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, parent.Pull.HeadSHA, parent.Pull.HeadSHA, "parent-branch"
			if afterAdmit {
				var err error
				child, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			for i, queue := range [][]int64{{4, 6}, {4, 2, 6}} {
				revision = int64(i + 2)
				c.Queue = queue
				if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprint("change", i), c); err != nil {
					t.Fatal(err)
				}
			}
			if afterAdmit {
				parent.Pull.HeadRef = "parent-branch"
				attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, parent), "validation_failed")
			} else {
				_, err := f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
				attemptCode(t, err, "review_conflict")
			}
		})
	}
}
