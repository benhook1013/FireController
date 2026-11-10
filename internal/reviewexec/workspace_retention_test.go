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
