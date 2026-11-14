package reviewexec

import (
	"testing"
	"time"
)

func TestWorkspaceRetentionProtectsLiveUncertainReservedAndRecentAttempts(t *testing.T) {
	for _, state := range []string{"preparing", "posting", "active", "uncertain", "completed", "held", "failed", "cancelled", "rate_limited"} {
		for _, channel := range []string{"cli", "hosted"} {
			t.Run(channel+"/"+state, func(t *testing.T) {
				f, a, _ := evidenceFixture(t, channel, true)
				// Exercise persisted eligibility independently of transition mechanics.
				for _, query := range []string{`UPDATE review_execution_attempts SET state=? WHERE id=?`, `UPDATE review_operations SET state=? WHERE attempt_id=?`, `UPDATE review_execution_transitions SET state=? WHERE attempt_id=? AND revision=2`} {
					if _, err := f.store.DB().Exec(query, state, a.ID); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Now().Add(10 * time.Minute)
				allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, now)
				if err != nil || allowed {
					t.Fatal("reservation must protect every state", allowed, err)
				}
				if _, err = f.store.DB().Exec(`DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
					t.Fatal(err)
				}
				allowed, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now())
				if err != nil || allowed {
					t.Fatal("recent attempt prematurely disposable", allowed, err)
				}
				terminal := state == "completed" || state == "held" || state == "failed" || state == "cancelled" || state == "rate_limited"
				allowed, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, now)
				if err != nil || allowed != terminal {
					t.Fatal(state, allowed, err)
				}
				allowed, err = f.e.WorkspaceDisposable(t.Context(), "other-account", a.ID, now)
				if err != nil || allowed {
					t.Fatal("cross-capability cleanup", allowed, err)
				}
			})
		}
	}
}

func TestWorkspaceRetentionReportsCorruptTransitionTime(t *testing.T) {
	f, a, _ := evidenceFixture(t, "cli", true)
	for _, query := range []string{
		`UPDATE review_execution_attempts SET state='completed' WHERE id=?`,
		`UPDATE review_operations SET state='completed' WHERE attempt_id=?`,
		`UPDATE review_execution_transitions SET state='completed',created_at='private-corrupt-time' WHERE attempt_id=? AND revision=2`,
		`DELETE FROM review_execution_reservations WHERE attempt_id=?`,
	} {
		if _, err := f.store.DB().Exec(query, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now().Add(time.Hour))
	attemptCode(t, err, "review_persistence_failed")
	if allowed || err.Error() != persistenceFailure().Error() {
		t.Fatal("corruption lost safe storage error", allowed, err)
	}
}

func TestWorkspaceRetentionProtectsDetachedCooldownIntentAndReservation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, protection := range []string{"intent", "reservation"} {
			t.Run(channel+"/"+protection, func(t *testing.T) {
				f, a := interruptedAllowanceFixture(t, channel)
				var err error
				if channel == "cli" {
					_, err = f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision)
				} else {
					_, err = f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, a.Revision)
				}
				if err != nil {
					t.Fatal(err)
				}
				later := time.Now().Add(time.Hour)
				if allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, later); err != nil || !allowed {
					t.Fatal("baseline detached cooldown is not disposable", allowed, err)
				}
				if protection == "intent" {
					admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='retained-dispatch-intent' WHERE id=?`, a.ID)
				} else {
					admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) SELECT a.provider,p.github_repository_id,o.pull_number,o.channel,a.account,a.id FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id JOIN projects p ON p.id=a.project_id WHERE a.id=?`, a.ID)
				}
				if allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, later); err != nil || allowed {
					t.Fatal("detached workspace protection ignored", allowed, err)
				}
			})
		}
	}
}

func TestWorkspaceRetentionRejectsParseableCorruptChronology(t *testing.T) {
	for _, damage := range []string{"zero", "offset", "before-start", "first-zero", "missing-first", "first-after-terminal"} {
		t.Run(damage, func(t *testing.T) {
			f, a, _ := evidenceFixture(t, "cli", true)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "completion", ""); err != nil {
				t.Fatal(err)
			}
			var first, terminal string
			if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID).Scan(&first); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=3`, a.ID).Scan(&terminal); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "zero":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at='0001-01-01T00:00:00Z' WHERE attempt_id=? AND revision=3`, a.ID)
			case "offset":
				at, _ := time.Parse(time.RFC3339Nano, terminal)
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=3`, at.In(time.FixedZone("offset", 3600)).Format(time.RFC3339Nano), a.ID)
			case "before-start":
				at, _ := time.Parse(time.RFC3339Nano, first)
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=3`, at.Add(-time.Hour).Format(time.RFC3339Nano), a.ID)
			case "first-zero":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at='0001-01-01T00:00:00Z' WHERE attempt_id=? AND revision=1`, a.ID)
			case "missing-first":
				admissionSQL(t, f, `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID)
			case "first-after-terminal":
				at, _ := time.Parse(time.RFC3339Nano, terminal)
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=1`, at.Add(time.Hour).Format(time.RFC3339Nano), a.ID)
			}
			allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now().Add(time.Hour))
			attemptCode(t, err, "review_persistence_failed")
			if allowed {
				t.Fatal("corrupt chronology authorized cleanup")
			}
		})
	}
}

func TestWorkspaceRetentionCollectsDetachedDraftAttempt(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", cliDraftWait, ""); err != nil {
		t.Fatal(err)
	}
	if disposable, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now()); err != nil || disposable {
		t.Fatal("recent detached Draft workspace", disposable, err)
	}
	if disposable, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now().Add(time.Hour)); err != nil || !disposable {
		t.Fatal("old detached Draft workspace leaked", disposable, err)
	}
	admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='dispatch-intent' WHERE id=?`, a.ID)
	if disposable, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now().Add(time.Hour)); err != nil || disposable {
		t.Fatal("detached dispatch intent was disposable", disposable, err)
	}
}
