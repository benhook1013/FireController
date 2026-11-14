package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func deadlineRuntimeFixture(t *testing.T, channel string) (*attemptFixture, Attempt, *time.Time) {
	t.Helper()
	if channel == "cli" {
		f, a, _, now := cliWaitFixture(t)
		if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
			t.Fatal(err)
		}
		return f, a, now
	}
	f, a, _, now := queuedCooldown(t)
	return f, a, now
}

func deadlineRuntimeState(t *testing.T, f *attemptFixture) string {
	t.Helper()
	var result strings.Builder
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_transitions", "review_operation_waits", "review_cli_preflight", "review_hosted_preflight", "review_execution_reservations", "events", "receipts"} {
		result.WriteString(snapshotTable(t, f, table))
	}
	return result.String()
}

func TestWaitRuntimeRejectsCanonicalDeadlineMismatch(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, change := range []string{"shortened", "extended"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				f, a, now := deadlineRuntimeFixture(t, channel)
				o := cliWaitRead(t, f, a.OperationID)
				due := cliWaitDue(t, o)
				forged := due.Add(-30 * time.Second)
				if change == "extended" {
					forged = due.Add(30 * time.Second)
				}
				admissionSQL(t, f, `UPDATE review_operation_waits SET retry_not_before=? WHERE operation_id=?`, forged.Format(time.RFC3339Nano), o.ID)
				*now = due.Add(-15 * time.Second)
				before := deadlineRuntimeState(t, f)
				if wait, err := readOperationWait(t.Context(), f.store.DB(), o); wait != nil || !isWaitUnavailable(err) {
					t.Error("canonical but contradictory deadline was accepted", wait, err)
				}
				if attempt, err := f.e.StartAttempt(t.Context(), attemptProject, o.ID); attempt.ID != "" || !isWaitUnavailable(err) {
					t.Error("direct admission trusted a contradictory timer", attempt, err)
				}
				if pending, err := f.e.pending(t.Context()); err != nil || len(pending) != 0 {
					t.Error("selection admitted a contradictory timer", pending, err)
				}
				if before != deadlineRuntimeState(t, f) {
					t.Fatal("failed runtime proof mutated retained state")
				}
			})
		}
	}
}

func TestWaitRuntimeBindsLatestCooldown(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		damages := []string{"old-deadline", "latest-proof"}
		if channel == "hosted" {
			damages = append(damages, "equal-ended-old-deadline", "equal-ended-latest-proof")
		}
		for _, damage := range damages {
			t.Run(channel+"/"+damage, func(t *testing.T) {
				f, first, now := deadlineRuntimeFixture(t, channel)
				baseline := cliWaitRead(t, f, first.OperationID)
				firstDue := cliWaitDue(t, baseline)
				var firstEnded string
				if strings.HasPrefix(damage, "equal-ended") {
					if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, first.ID).Scan(&firstEnded); err != nil {
						t.Fatal(err)
					}
					f.e.attemptNow = func() time.Time { return mustWaitTime(firstEnded) }
				}
				*now = firstDue
				next := f.start(t, attemptProject, Receipt{ID: first.OperationID})
				// Projection reads the operation in the same transaction before
				// the newly held preparation is detached from it.
				f.e.publishAttempt = func(ctx context.Context, tx *sql.Tx, a Attempt) error {
					_, err := readOperationBase(ctx, tx, a.ProjectID, a.OperationID)
					return err
				}
				var due time.Time
				if channel == "cli" {
					in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 7, FullCapacityInMS: 3600000, RollingWindowMS: 86400000, ChangedFiles: 2}
					if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, next.ID, next.Revision, in); err != nil {
						t.Fatal(err)
					}
					if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, next.ID, next.Revision); err != nil {
						t.Fatal("second cooldown with in-transaction projection", err)
					}
					due = firstDue.Add(2 * time.Minute)
				} else {
					in := retainTimerCooldown(t, f, next, 6*time.Minute)
					if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, next.ID, next.Revision); err != nil {
						t.Fatal("second cooldown with in-transaction projection", err)
					}
					due = in.Observation.RetryNotBefore
				}
				o := cliWaitRead(t, f, first.OperationID)
				if o.Wait.BaselineAttemptID != first.ID || !cliWaitDue(t, o).Equal(due) || !due.After(firstDue) {
					t.Fatal("latest timer replaced first comparison or retained old deadline", o.Wait, firstDue, due)
				}
				if channel == "cli" && (o.Wait.CLIAllowance == nil || *o.Wait.CLIAllowance.Limit != *baseline.Wait.CLIAllowance.Limit) {
					t.Fatal("latest scheduling proof replaced the first allowance estimate")
				}
				if firstEnded != "" {
					var latestEnded string
					if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, next.ID).Scan(&latestEnded); err != nil || latestEnded != firstEnded {
						t.Fatal("fixture did not retain equal cooldown transition times", firstEnded, latestEnded, err)
					}
				}
				if strings.HasSuffix(damage, "old-deadline") {
					admissionSQL(t, f, `UPDATE review_operation_waits SET retry_not_before=? WHERE operation_id=?`, firstDue.Format(time.RFC3339Nano), o.ID)

				} else {
					admissionSQL(t, f, `UPDATE review_`+channel+`_preflight SET body_json='{}' WHERE attempt_id=?`, next.ID)
				}
				before := deadlineRuntimeState(t, f)
				if wait, err := readOperationWait(t.Context(), f.store.DB(), o); wait != nil || !isWaitUnavailable(err) {
					t.Error("first baseline concealed an unproven latest cooldown", wait, err)
				}
				if before != deadlineRuntimeState(t, f) {
					t.Fatal("latest cooldown validation changed retained state")
				}
			})
		}
	}
}

func TestCLIAccountDeadlineCannotHideShortenedCooldown(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		otherAccount bool
		expired      bool
		extended     bool
	}{
		{name: "same-live"},
		{name: "other-live", otherAccount: true},
		{name: "same-expired", expired: true},
		{name: "other-expired", otherAccount: true, expired: true},
		{name: "extended-after-real-expiry", expired: true, extended: true},
		{name: "other-extended-after-real-expiry", otherAccount: true, expired: true, extended: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, a, now := deadlineRuntimeFixture(t, "cli")
			due := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			capability := "test-capability"
			if scenario.otherAccount {
				capability = "other-account"
			}
			f.configure(t, otherAttemptProject, capability, 0, false)
			other := f.enqueue(t, otherAttemptProject, "shortened-account-timer", "cli", 2, 1)
			hosted := f.enqueue(t, otherAttemptProject, "independent-hosted-timer", "hosted", 4, 1)
			timerControl(t, f, a.OperationID, "cancel", "cancel-shortened-donor")
			forged := due.Add(-30 * time.Second)
			if scenario.extended {
				forged = due.Add(30 * time.Second)
			}
			admissionSQL(t, f, `UPDATE review_operation_waits SET retry_not_before=? WHERE operation_id=?`, forged.Format(time.RFC3339Nano), a.OperationID)
			*now = due.Add(-15 * time.Second)
			if scenario.expired {
				*now = due
				if !scenario.extended {
					admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json='{}' WHERE attempt_id=?`, a.ID)
				}
			}
			before := deadlineRuntimeState(t, f)
			blocked := !scenario.otherAccount && (!scenario.expired || scenario.extended)
			deadline, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities[capability], *now)
			if blocked {
				if !deadline.IsZero() || !isWaitUnavailable(err) {
					t.Error("shortened cache hid a live account exhaustion", deadline, err)
				}
			} else if !deadline.IsZero() || err != nil {
				t.Error("unrelated or expired corruption became a live account hold", deadline, err)
			}
			pending, err := f.e.pending(t.Context())
			if err != nil {
				t.Fatal("account damage stopped unrelated scheduling", err)
			}
			got := map[string]bool{}
			for _, operation := range pending {
				got[operation.ID] = true
			}
			want := 2
			if blocked {
				want = 1
			}
			if !got[hosted.ID] || got[other.ID] == blocked || len(got) != want {
				t.Error("account deadline lost scheduling isolation", got)
			}
			if before != deadlineRuntimeState(t, f) {
				t.Fatal("account lookup changed retained state")
			}
			attempt, err := f.e.StartAttempt(t.Context(), otherAttemptProject, other.ID)
			if blocked {
				if attempt.ID != "" || !isWaitUnavailable(err) {
					t.Error("direct cross-project admission bypassed retained floor", attempt, err)
				}
			} else if attempt.ID == "" || err != nil {
				t.Error("unrelated or truly expired timer blocked admission", attempt, err)
			}
		})
	}
}

func TestWaitDeadlineRuntimePreservesLookupErrors(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f, a, _ := deadlineRuntimeFixture(t, channel)
			o := cliWaitRead(t, f, a.OperationID)
			before := deadlineRuntimeState(t, f)
			for _, match := range []string{"FROM review_execution_attempts a", "SELECT created_at FROM review_execution_transitions"} {
				for _, canceled := range []bool{false, true} {
					q := cliRetryFailureQuery{EvidenceQuery: f.store.DB(), match: match, cancel: canceled}
					wait, err := readOperationWait(t.Context(), q, o)
					if wait != nil || err == nil || isWaitUnavailable(err) || canceled && !errors.Is(err, context.Canceled) || !canceled && !strings.Contains(err.Error(), "missing_cli_retry_history") {
						t.Error("latest deadline proof concealed an operational lookup failure", match, canceled, wait, err)
					}
				}
			}
			if before != deadlineRuntimeState(t, f) {
				t.Fatal("failed proof lookup changed retained state")
			}
		})
	}
}

func TestHostedTiedCooldownCannotHideMalformedMiddleProof(t *testing.T) {
	f, first, now := deadlineRuntimeFixture(t, "hosted")
	var firstEnded string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, first.ID).Scan(&firstEnded); err != nil {
		t.Fatal(err)
	}
	f.e.attemptNow = func() time.Time { return mustWaitTime(firstEnded).Add(-time.Hour) }
	var middle Attempt
	for i := 0; i < 2; i++ {
		*now = cliWaitDue(t, cliWaitRead(t, f, first.OperationID))
		next := f.start(t, attemptProject, Receipt{ID: first.OperationID})
		retainTimerCooldown(t, f, next, time.Duration(6+i*6)*time.Minute)
		if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, next.ID, 1); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			middle = next
		}
	}
	o := cliWaitRead(t, f, first.OperationID)
	admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json=json_set(body_json,'$.input.observation.RetryNotBefore','!') WHERE attempt_id=?`, middle.ID)
	before := deadlineRuntimeState(t, f)
	if wait, err := readOperationWait(t.Context(), f.store.DB(), o); wait != nil || !isWaitUnavailable(err) {
		t.Fatal("malformed equal-time proof hidden", wait, err)
	}
	if before != deadlineRuntimeState(t, f) {
		t.Fatal("read mutated evidence")
	}
}
