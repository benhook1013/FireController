package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestSchedulingCLI0030MalformedOperationIsolation(t *testing.T) {
	for _, account := range []string{"test-capability", "other-account"} {
		for _, field := range []string{"revision", "configuration_revision", "policy_revision", "pull_number", "allow_out_of_order", "override_file_limit"} {
			t.Run(account+"/"+field, func(t *testing.T) {
				f := schedulingFixture(t)
				f.configure(t, otherAttemptProject, account, 0, false)
				low := f.enqueue(t, attemptProject, "healthy", "cli", 2, 1)
				high := f.enqueue(t, otherAttemptProject, "malformed", "cli", 2, 1)
				admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, otherAttemptProject)
				schedulingCLI0030CorruptOperation(t, f, field, high.ID)
				err := f.store.Read(t.Context(), f.scope(t, otherAttemptProject, f.admin, true), func(ctx context.Context, tx *sql.Tx) error {
					_, err := readOperationBase(ctx, tx, otherAttemptProject, high.ID)
					if !isRetainedOperationDecodeError(err) {
						t.Fatalf("malformed %s not classified: %v", field, err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID)
				if err != nil || a.ID == "" {
					t.Fatal("malformed preferred candidate blocked healthy attempt", a, err)
				}
			})
		}
	}
}

// Model retained damage independently of the live writer's SQL constraints.
func schedulingCLI0030CorruptOperation(t *testing.T, f *attemptFixture, field, id string) {
	t.Helper()
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, query := range []string{`PRAGMA foreign_keys=OFF`, `PRAGMA ignore_check_constraints=ON`} {
		if _, err := conn.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, query := range []string{`PRAGMA ignore_check_constraints=OFF`, `PRAGMA foreign_keys=ON`} {
			if _, err := conn.ExecContext(t.Context(), query); err != nil {
				t.Error(err)
			}
		}
	}()
	if _, err := conn.ExecContext(t.Context(), `UPDATE review_operations SET `+field+`='malformed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulingCLI0030OperationReadInfrastructureErrors(t *testing.T) {
	for _, fault := range []string{"query", "transaction", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixture(t)
			op := f.enqueue(t, attemptProject, "healthy", "cli", 2, 1)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx := t.Context()
			switch fault {
			case "query":
				// Metadata still reads successfully; the operation SELECT itself fails.
				if _, err := tx.ExecContext(ctx, `ALTER TABLE review_operations RENAME COLUMN revision TO missing_revision`); err != nil {
					t.Fatal(err)
				}
			case "transaction":
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err = readOperationBase(ctx, tx, attemptProject, op.ID)
			if err == nil || isRetainedOperationDecodeError(err) {
				t.Fatal("infrastructure error suppressed or classified as retained data", err)
			}
			if fault == "transaction" && !errors.Is(err, sql.ErrTxDone) {
				t.Fatal(err)
			}
			if fault == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func schedulingCLI0030Priority(t *testing.T, f *attemptFixture, project, id string) bool {
	t.Helper()
	var preferred bool
	err := f.store.Read(t.Context(), f.scope(t, project, f.admin, true), func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		c, _, err := readConfiguration(ctx, tx, project)
		if err != nil {
			return err
		}
		preferred, err = f.e.preferredCLIQueued(ctx, tx, o, c)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return preferred
}

func TestSchedulingCLI0030CapacityBlockedSelectionDoesNotReserveAccount(t *testing.T) {
	for _, capacity := range []string{"project", "global"} {
		t.Run(capacity, func(t *testing.T) {
			f := selectionFixtureWithQueue(t, []int64{2})
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			low := f.enqueue(t, otherAttemptProject, "healthy", "cli", 2, 1)
			selected, _ := selectionEnqueue(t, f, "preferred", "cli", nil)
			admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, attemptProject)
			if !schedulingCLI0030Priority(t, f, otherAttemptProject, low.ID) {
				t.Fatal("eligible preferred selection did not receive priority")
			}
			occupied := attemptProject
			if capacity == "project" {
				selectionCapacityFill(t, f, occupied)
			} else {
				for i := range requestGlobalCapacity / requestProjectCapacity {
					project := fmt.Sprintf("priority-capacity-%d", i)
					selectionCapacityProject(t, f, project, int64(900+i))
					selectionCapacityFill(t, f, project)
					admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE project_id=?`, project)
					occupied = project
				}
				// 256 synthetic operations plus the real competing operation: release
				// one to reach exactly the global limit, with preferred local room.
				selectionCapacityRelease(t, f, occupied)
			}
			if schedulingCLI0030Priority(t, f, otherAttemptProject, low.ID) {
				t.Fatal("capacity-blocked selection reserved account")
			}
			worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
			if err != nil || worked {
				t.Fatal("capacity-blocked selection was claimable", worked, err)
			}
			// Release one pending operation. Priority returns as soon as the
			// ordinary project/global capacity rule allows selection to progress.
			releaseID := "capacity-" + occupied + "-2"
			admissionSQL(t, f, `UPDATE review_operations SET state='held' WHERE id=?`, releaseID)
			if !schedulingCLI0030Priority(t, f, otherAttemptProject, low.ID) {
				t.Fatal("priority did not return after capacity release")
			}
			admissionSQL(t, f, `UPDATE review_operations SET state='queued' WHERE id=?`, releaseID)
			a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID)
			if err != nil || a.ID == "" {
				t.Fatal("full queue prevented competing attempt from draining", a, err)
			}
			r := selectionRead(t, f, selected.ID)
			if r.State != "queued" || r.Revision != 1 || r.OperationID != "" {
				t.Fatal("priority check altered preferred selection", r)
			}
		})
	}
}
