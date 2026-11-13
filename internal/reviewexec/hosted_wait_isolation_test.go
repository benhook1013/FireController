package reviewexec

import (
	"context"
	"testing"
	"time"
)

func TestHostedWaitHistoryIsolatesProjectAndChannel(t *testing.T) {
	for _, due := range []bool{false, true} {
		for _, dependency := range []string{"preflight", "query"} {
			name := dependency + "/predecessor"
			if due {
				name = dependency + "/candidate"
			}
			t.Run(name, func(t *testing.T) {
				f, a, cooldown, now := queuedCooldown(t)
				f.configure(t, otherAttemptProject, "test-capability", 0, false)
				blocked := f.enqueue(t, attemptProject, "later-hosted", "hosted", 4, 1)
				cli := f.enqueue(t, attemptProject, "healthy-cli", "cli", 2, 1)
				hosted := f.enqueue(t, otherAttemptProject, "healthy-hosted", "hosted", 2, 1)
				table := "review_hosted_preflight"
				if dependency == "query" {
					table = "review_hosted_allowance_queries"
				}
				admissionSQL(t, f, "UPDATE "+table+" SET body_json='private-damaged-wait' WHERE attempt_id=?", a.ID)
				*now = cooldown.Observation.RetryNotBefore.Add(-time.Second)
				if due {
					*now = cooldown.Observation.RetryNotBefore
				}
				tables := []string{"review_operations", "review_execution_attempts", "review_operation_waits", table, "review_execution_reservations", "events"}
				before := map[string]string{}
				for _, table := range tables {
					before[table] = snapshotTable(t, f, table)
				}
				for range 2 {
					pending, err := f.e.pending(t.Context())
					if err != nil {
						t.Fatal("damaged Hosted wait stopped selection", err)
					}
					got := map[string]bool{}
					for _, o := range pending {
						got[o.ID] = true
					}
					if len(got) != 2 || !got[cli.ID] || !got[hosted.ID] || got[blocked.ID] || got[a.OperationID] {
						t.Fatal("wait scope or predecessor priority lost", got)
					}
				}
				for _, table := range tables {
					if before[table] != snapshotTable(t, f, table) {
						t.Fatal("selection mutated retained state", table)
					}
				}
				seen := map[string]int{}
				driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
					seen[o.ID]++
					return Preparation{}, sourceHold("synthetic_isolation_hold")
				}}
				for range 2 {
					if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
						t.Fatal("damaged Hosted wait stopped healthy work", err)
					}
				}
				if seen[cli.ID] != 1 || seen[hosted.ID] != 1 || seen[blocked.ID] != 0 || seen[a.OperationID] != 0 || driver.calls.Load() != 0 {
					t.Fatal("healthy work or damaged scope changed", seen)
				}
				if supervisorOperationState(t, f, a.OperationID) != "queued" || supervisorOperationState(t, f, blocked.ID) != "queued" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("damaged wait changed lifecycle or acquired capacity")
				}
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("continuation rewrote damaged evidence")
				}
			})
		}
	}
}

func TestHostedWaitLookupFailureRemainsFatal(t *testing.T) {
	f, _, cooldown, now := queuedCooldown(t)
	*now = cooldown.Observation.RetryNotBefore
	admissionSQL(t, f, `DROP TABLE review_hosted_preflight`)
	if _, err := f.e.pending(t.Context()); err == nil || isWaitUnavailable(err) {
		t.Fatal("database failure suppressed or relabeled", err)
	}
	err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": &controlledDriver{}})
	attemptCode(t, err, "review_persistence_failed")
}
