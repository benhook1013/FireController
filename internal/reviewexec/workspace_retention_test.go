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
