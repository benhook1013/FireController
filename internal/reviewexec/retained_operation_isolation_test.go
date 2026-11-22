package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func retainedOperationProjectState(t *testing.T, f *attemptFixture, project string) string {
	t.Helper()
	queries := []string{
		`SELECT * FROM review_operations WHERE project_id=? ORDER BY id`,
		`SELECT * FROM review_execution_attempts WHERE project_id=? ORDER BY id`,
		`SELECT r.* FROM review_execution_reservations r
		  JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE a.project_id=? ORDER BY r.attempt_id`,
		`SELECT tr.* FROM review_execution_transitions tr
		  JOIN review_execution_attempts a ON a.id=tr.attempt_id WHERE a.project_id=? ORDER BY tr.attempt_id,tr.revision`,
		`SELECT w.* FROM review_operation_waits w
		  JOIN review_operations o ON o.id=w.operation_id WHERE o.project_id=? ORDER BY w.operation_id`,
		`SELECT * FROM receipts WHERE project_id=? ORDER BY instance_id,principal_id,operation,target,key`,
		`SELECT * FROM events WHERE project_id=? ORDER BY sequence`,
	}
	var snapshot strings.Builder
	for _, query := range queries {
		rows, err := f.store.DB().QueryContext(t.Context(), query, project)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintln(&snapshot, query)
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range destinations {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			fmt.Fprintf(&snapshot, "%#v\n", values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot.String()
}

func TestMalformedRetainedOperationIsolatesPendingCandidate(t *testing.T) {
	for _, tc := range []struct {
		field, statement string
	}{
		{"actor", `UPDATE review_operations SET actor_id=CAST(actor_id AS BLOB),created_at='2020-01-01T00:00:00Z' WHERE id=?`},
		{"predecessor-key", `UPDATE review_operations SET id=CAST(id AS BLOB),created_at='2020-01-01T00:00:00Z' WHERE id=?`},
	} {
		for _, channel := range []string{"cli", "hosted"} {
			t.Run(channel+"/"+tc.field, func(t *testing.T) {
				f := supervisorFixture(t)
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				damaged := f.enqueue(t, attemptProject, "unproven-operation", channel, 2, 1)
				f.enqueue(t, attemptProject, "blocked-predecessor", channel, 4, 1)
				healthy := f.enqueue(t, otherAttemptProject, "independent-operation", channel, 2, 1)
				admissionSQL(t, f, tc.statement, damaged.ID)
				before := retainedOperationProjectState(t, f, attemptProject)
				driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
					if o.ID != healthy.ID {
						t.Errorf("unexpected operation reached preparation: %s", o.ID)
					}
					return Preparation{}, sourceHold("fixture_stop")
				}}
				for range 2 {
					if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
						t.Fatal(err)
					}
				}
				if driver.prepares.Load() != 1 || driver.calls.Load() != 0 ||
					retainedOperationProjectState(t, f, attemptProject) != before {
					t.Fatal("damage blocked independent work, changed retained state, or reached dispatch")
				}
				receipt, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "unproven-operation", Request{
					ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40),
				})
				if err != nil || string(receipt) != encoded(damaged) || retainedOperationProjectState(t, f, attemptProject) != before {
					t.Fatal("original-key replay changed the operation or its receipt", err)
				}
			})
		}
	}
}

func TestMalformedRetainedOperationIsolatesDraftReread(t *testing.T) {
	for _, fault := range []string{"native-value", "shared-query"} {
		t.Run(fault, func(t *testing.T) {
			f := supervisorFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			bad := f.enqueue(t, attemptProject, "draft-reread", "cli", 2, 1)
			good := f.enqueue(t, otherAttemptProject, "next-draft-reread", "cli", 2, 1)
			goodOperation, err := readOperation(t.Context(), f.store.DB(), otherAttemptProject, good.ID)
			if err != nil {
				t.Fatal(err)
			}
			batch := []Operation{roundOperation(t, f, bad.ID), goodOperation}
			if fault == "native-value" {
				admissionSQL(t, f, `UPDATE review_operations SET actor_id=CAST(actor_id AS BLOB) WHERE id=?`, bad.ID)
			} else {
				admissionSQL(t, f, `ALTER TABLE review_operations RENAME COLUMN actor_id TO missing_actor_id`)
			}
			before := retainedOperationProjectState(t, f, attemptProject)
			otherBefore := retainedOperationProjectState(t, f, otherAttemptProject)
			driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				if o.ID != good.ID {
					t.Errorf("unexpected operation reached preparation: %s", o.ID)
				}
				return Preparation{}, sourceHold("fixture_stop")
			}}
			err = f.e.processPendingBatch(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}, func() {}, 0, batch, pendingExhaustion{})
			if retainedOperationProjectState(t, f, attemptProject) != before || driver.calls.Load() != 0 {
				t.Fatal("damaged operation or its journal changed")
			}
			if fault == "shared-query" {
				attemptCode(t, err, "review_persistence_failed")
				if driver.prepares.Load() != 0 || retainedOperationProjectState(t, f, otherAttemptProject) != otherBefore {
					t.Fatal("shared query failure allowed another claim")
				}
			} else if err != nil || driver.prepares.Load() != 1 {
				t.Fatal("draft reread did not continue to the independent candidate", err)
			}
		})
	}
}

func TestMalformedRetainedOperationPreservesPreparationReservation(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	bad := f.enqueue(t, attemptProject, "preparation-reread", "hosted", 2, 1)
	good := f.enqueue(t, otherAttemptProject, "independent-preparation", "hosted", 2, 1)
	var protected string
	driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		switch o.ID {
		case bad.ID:
			admissionSQL(t, f, `UPDATE review_operations SET actor_id=CAST(actor_id AS BLOB) WHERE id=?`, bad.ID)
			protected = retainedOperationProjectState(t, f, attemptProject)
			_, err := f.e.ExecutionContext(ctx, o.ProjectID, o.ID)
			if err == nil {
				t.Error("driver received a usable execution context for corrupt operation")
			}
			return Preparation{}, err
		case good.ID:
			return Preparation{}, sourceHold("fixture_stop")
		default:
			t.Fatalf("unexpected operation reached preparation: %s", o.ID)
			return Preparation{}, nil
		}
	}}
	goodOperation, err := readOperation(t.Context(), f.store.DB(), otherAttemptProject, good.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch := []Operation{roundOperation(t, f, bad.ID), goodOperation}
	err = f.e.processPendingBatch(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}, func() {}, 0, batch, pendingExhaustion{})
	if err != nil || driver.prepares.Load() != 2 || driver.calls.Load() != 0 || protected == "" ||
		retainedOperationProjectState(t, f, attemptProject) != protected {
		t.Fatal("corrupt preparation lost its reservation or blocked independent work", err)
	}
}

func TestMalformedRetainedOperationIsolatesRestartRecovery(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := supervisorFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			bad := f.start(t, attemptProject, f.enqueue(t, attemptProject, "restart-damage", channel, 2, 1))
			healthy := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "restart-healthy", "cli", 2, 1))
			admissionSQL(t, f, `UPDATE review_operations SET actor_id=CAST(actor_id AS BLOB) WHERE id=?`, bad.OperationID)
			before := retainedOperationProjectState(t, f, attemptProject)
			skipped, err := f.e.reconcilePreparingDrivers(t.Context(), nil)
			if err != nil || skipped[bad.ID] != (channel == "hosted") {
				t.Fatal("unexpected preparation recovery classification", skipped, err)
			}
			for range 2 {
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readAttempt(t.Context(), f.store.DB(), healthy.ProjectID, healthy.ID)
			if err != nil || got.State != "cancelled" {
				t.Fatal("healthy preparation did not follow ordinary interruption recovery", got, err)
			}
			if retainedOperationProjectState(t, f, attemptProject) != before {
				t.Fatal("unproven recovery candidate or reservation changed")
			}
		})
	}
}

func TestRetainedOperationScannerRequiresNativeSQLiteValues(t *testing.T) {
	for _, value := range []any{[]byte("queued"), int64(1), nil} {
		var destination string
		if err := operationValue(&destination).Scan(value); !isRetainedOperationDecodeError(err) {
			t.Fatalf("accepted non-TEXT operation field %T", value)
		}
	}
	for _, value := range []any{"1", []byte("1"), float64(1), nil} {
		var destination int64
		if err := operationValue(&destination).Scan(value); !isRetainedOperationDecodeError(err) {
			t.Fatalf("accepted non-INTEGER operation field %T", value)
		}
	}
	for _, value := range []any{true, "true", "1", []byte("1"), float64(1), int64(2), nil} {
		var destination bool
		if err := operationValue(&destination).Scan(value); !isRetainedOperationDecodeError(err) {
			t.Fatalf("accepted non-native boolean operation field %T", value)
		}
	}
	var text string
	var integer int64
	var flag bool
	if operationValue(&text).Scan("queued") != nil || text != "queued" || operationValue(&integer).Scan(int64(3)) != nil || integer != 3 || operationValue(&flag).Scan(int64(1)) != nil || !flag || operationValue(&flag).Scan(int64(0)) != nil || flag {
		t.Fatal("rejected native SQLite operation values")
	}
}

func TestRetainedOperationReadDoesNotClassifyInfrastructureFailures(t *testing.T) {
	for _, fault := range []string{"schema-version", "query", "transaction", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			f := supervisorFixture(t)
			op := f.enqueue(t, attemptProject, "infrastructure", "cli", 2, 1)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx := t.Context()
			var want error
			switch fault {
			case "schema-version":
				_, err = tx.ExecContext(ctx, `UPDATE metadata SET value='invalid' WHERE key='schema:review_execution'`)
			case "query":
				_, err = tx.ExecContext(ctx, `ALTER TABLE review_operations RENAME COLUMN actor_id TO missing_actor_id`)
			case "transaction":
				err = tx.Rollback()
				want = sql.ErrTxDone
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = readOperationIdentity(ctx, tx, attemptProject, op.ID)
			if err == nil || isRetainedOperationDecodeError(err) || want != nil && !errors.Is(err, want) {
				t.Fatal("infrastructure failure became candidate-local or was lost", err)
			}
		})
	}
}
