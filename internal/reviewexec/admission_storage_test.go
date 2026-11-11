package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	holdmodel "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type admissionStorageCode int

func (e admissionStorageCode) Error() string { return "private SQL failure" }
func (e admissionStorageCode) Code() int     { return int(e) }

func TestReadAdmissionStorageClassification(t *testing.T) {
	f := selectionFixture(t)
	for _, tc := range []struct {
		failure error
		code    string
	}{
		{admissionStorageCode(5), "overloaded"},
		{admissionStorageCode(6), "overloaded"},
		{context.Canceled, "overloaded"},
		{context.DeadlineExceeded, "overloaded"},
		{core.Fail("overloaded", "Safe overload.", 503), "overloaded"},
		{errors.New("private unknown SQL failure"), "storage_unavailable"},
	} {
		err := f.e.readAdmission(t.Context(), func(context.Context, *sql.Tx) error { return tc.failure })
		attemptCode(t, err, tc.code)
		if strings.Contains(err.Error(), "private") {
			t.Fatal("raw SQL diagnostic exposed", err)
		}
	}
}

func TestReadAdmissionCancellationRollsBackDespiteNilCallbackError(t *testing.T) {
	f := selectionFixture(t)
	var before string
	if err := f.store.DB().QueryRow(`SELECT name FROM projects WHERE id=?`, attemptProject).Scan(&before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := f.e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='must-rollback' WHERE id=?`, attemptProject); err != nil {
			return err
		}
		cancel()
		return nil
	})
	attemptCode(t, err, "overloaded")
	var after string
	if err := f.store.DB().QueryRow(`SELECT name FROM projects WHERE id=?`, attemptProject).Scan(&after); err != nil || after != before {
		t.Fatal("cancelled read transaction changed retained state", after, err)
	}
}

type admissionObligationFailure struct {
	*sql.Tx
	failure error
}

func (q admissionObligationFailure) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if q.failure != nil && strings.Contains(query, "SELECT obligation_id") {
		return nil, q.failure
	}
	return q.Tx.QueryContext(ctx, query, args...)
}

func TestAdmissionObligationTransientRetriesWithoutFalseHold(t *testing.T) {
	for _, failure := range []error{admissionStorageCode(5), admissionStorageCode(6), core.Fail("overloaded", "Safe transient failure.", 503)} {
		f := selectionFixture(t)
		retainObligationSource(t, f, attemptProject, "transient-importer", "transient-source", strings.Repeat("c", 64), 0)
		// Resolve normally, then retry against the complete retained graph for
		// that same PR rather than relying on an empty unrelated target list.
		var id, original string
		if err := f.store.DB().QueryRow(`SELECT id,original_json FROM review_adoption_obligations WHERE project_id=?`, attemptProject).Scan(&id, &original); err != nil {
			t.Fatal(err)
		}
		one := int64(1)
		if _, err := adoptionholds.New(f.store).Resolve(t.Context(), obligationSourceScope(t, f, attemptProject, "transient-resolver"), id, "resolve-for-retry", holdmodel.ResolveRequest{ExpectedRevision: &one, Reason: "Correction verified", Evidence: []string{"proof:correction"}}); err != nil {
			t.Fatal(err)
		}
		beforeSource, err := adoptionholds.ReadSource(t.Context(), f.store.DB(), attemptProject, "transient-source")
		if err != nil {
			t.Fatal(err)
		}
		reader := &refreshTestReader{}
		repository, err := reader.Repository(t.Context(), 801)
		if err != nil {
			t.Fatal(err)
		}
		pull, err := reader.Pull(t.Context(), repository.FullName, 2)
		if err != nil {
			t.Fatal(err)
		}
		plan := SourcePlan{Repository: repository, Pull: pull, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
		calls := 0
		err = retryRunSelectionLocal(t.Context(), func() error {
			calls++
			return f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var injected error
				if calls == 1 {
					injected = failure
				}
				facts, err := adoptionholds.ReadFacts(ctx, admissionObligationFailure{Tx: tx, failure: injected}, attemptProject, 2)
				if err != nil {
					return err
				}
				if facts.Open != 0 {
					t.Fatal("resolved target acquired a hold", facts)
				}
				front, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, []SourcePlan{plan}, false)
				if err != nil {
					return err
				}
				if front.state != "ready" || front.plan == nil || front.plan.Pull.Number != 2 {
					t.Fatal("retry produced false front hold", front)
				}
				return nil
			})
		})
		if err != nil || calls != 2 {
			t.Fatal("transient became terminal instead of retrying", calls, err)
		}
		var after string
		if err := f.store.DB().QueryRow(`SELECT original_json FROM review_adoption_obligations WHERE project_id=?`, attemptProject).Scan(&after); err != nil || original != after {
			t.Fatal("retry changed retained source facts", err)
		}
		afterSource, err := adoptionholds.ReadSource(t.Context(), f.store.DB(), attemptProject, "transient-source")
		if err != nil || beforeSource != afterSource {
			t.Fatal("retry changed retained source", err)
		}
		if _, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "enqueue-after-retry", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: pull.HeadSHA, Channel: "cli"}); err != nil {
			t.Fatal("normal admission failed after retry", err)
		}
		if reader.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 0 {
			t.Fatal("retry duplicated source/provider work or created terminal selection", reader.calls.Load())
		}
	}
}

func TestFrontOperationalQueryFailureDoesNotBecomeHeld(t *testing.T) {
	f := selectionFixture(t)
	var beforeVersion string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	retainObligationSource(t, f, attemptProject, "query-failure-importer", "query-failure-source", strings.Repeat("c", 64), 0)
	err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// Remove the queried table only inside this disposable transaction. The
		// real evaluator must return the SQL failure, not a nil-error held result.
		if _, err := tx.ExecContext(ctx, `DROP TABLE metadata`); err != nil {
			return err
		}
		plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}, ParentSHA: strings.Repeat("b", 40)}
		front, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, []SourcePlan{plan}, false)
		var failure *core.Error
		if err == nil || front.state != "" || errors.As(err, &failure) {
			t.Fatal("query failure became a successful/protective front", front, err)
		}
		return err
	})
	attemptCode(t, err, "storage_unavailable")
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&version); err != nil || version != beforeVersion {
		t.Fatal("failed read did not roll back fixture change", version, err)
	}
}

// Hold the actual read pool through its first two-second snapshot deadline.
// The supervisor must call Prepare once, while the execution-context read alone
// retries; provider preparation and dispatch must remain outside that retry.
func TestExecutionContextOverloadRetriesOnlyLocalPreparationRead(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, outcome := range []string{"recovered", "revoked", "cancelled", "deadline", "exhausted"} {
			t.Run(channel+"/"+outcome, func(t *testing.T) {
				t.Parallel()
				f := selectionFixture(t)
				receipt := f.enqueue(t, attemptProject, "execution-read-retry", channel, 2, 1)
				f.store.DB().SetMaxOpenConns(1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered := make(chan *sql.Conn, 1)
				var providerPreparation atomic.Int32
				driver := &controlledDriver{prepare: func(ctx context.Context, operation Operation, _ Configuration, attempt Attempt) (Preparation, error) {
					conn, err := f.store.DB().Conn(ctx)
					if err != nil {
						return Preparation{}, err
					}
					defer conn.Close()
					if outcome == "deadline" {
						var deadlineCancel context.CancelFunc
						ctx, deadlineCancel = context.WithTimeout(ctx, 100*time.Millisecond)
						defer deadlineCancel()
					}
					entered <- conn
					current, err := f.e.ExecutionContext(ctx, operation.ProjectID, operation.ID)
					if err != nil {
						return Preparation{}, err
					}
					if current.Operation.ID != operation.ID || current.Operation.AttemptID != attempt.ID || current.Operation.State != "preparing" {
						t.Error("retry lost the original operation binding", current.Operation)
					}
					providerPreparation.Add(1)
					intent := ""
					if channel == "hosted" {
						intent = "synthetic-intent-" + attempt.ID
					}
					return Preparation{Anchor: attemptAnchor(operation.ProjectID, operation.PullNumber), Intent: intent}, nil
				}}
				driver.dispatch = func(context.Context, Operation, Attempt, func(context.Context, string) error) (Result, error) {
					result := Result{State: "completed", Reason: "synthetic_capture"}
					if channel == "hosted" {
						result.ExternalID = "synthetic-trigger"
					}
					return result, nil
				}
				done := make(chan error, 1)
				go func() {
					done <- f.e.processNext(ctx, map[string]Driver{"test-capability": driver})
					close(done)
				}()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("execution-context retry leaked its supervisor")
					}
				})
				var conn *sql.Conn
				select {
				case conn = <-entered:
				case err := <-done:
					t.Fatal("preparation did not reach local read", err)
				case <-time.After(3 * time.Second):
					t.Fatal("preparation did not start")
				}
				defer conn.Close()
				waitSupervisor(t, "execution read entered pool", func() bool { return f.store.DB().Stats().WaitCount >= 1 })
				if outcome == "recovered" || outcome == "revoked" || outcome == "cancelled" {
					select {
					case err := <-done:
						t.Fatal("transient local read became terminal instead of retrying", err)
					case <-time.After(2200 * time.Millisecond):
					}
					if driver.prepares.Load() != 1 || providerPreparation.Load() != 0 || driver.calls.Load() != 0 || f.store.DB().Stats().WaitCount < 2 {
						t.Fatal("retry repeated preparation, reached provider work or never retried the read")
					}
					if outcome == "revoked" {
						if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
							_, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "cancelled" {
						cancel()
					} else if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
				}
				completionBudget := 4 * time.Second
				if outcome == "cancelled" || outcome == "deadline" {
					completionBudget = time.Second
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal("supervisor failed to retain local-read outcome", err)
					}
				case <-time.After(completionBudget):
					t.Fatal("local-read retry exceeded aggregate or caller bound")
				}
				conn.Close()
				var state, reason string
				if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_operations WHERE id=?`, receipt.ID).Scan(&state, &reason); err != nil {
					t.Fatal(err)
				}
				wantState, wantReason, wantProvider := "held", "preparation_failed", int32(0)
				if outcome == "recovered" {
					wantState, wantReason, wantProvider = "completed", "synthetic_capture", 1
				} else if outcome == "cancelled" {
					wantState, wantReason = "cancelled", "supervisor_stopping"
				}
				if state != wantState || reason != wantReason || driver.prepares.Load() != 1 || providerPreparation.Load() != wantProvider || driver.calls.Load() != wantProvider {
					t.Fatal("retry changed preparation or terminal semantics", state, reason, driver.prepares.Load(), providerPreparation.Load(), driver.calls.Load())
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("retry duplicated retained work or leaked the reservation")
				}
			})
		}
	}
}

func TestExecutionContextNonOverloadFailureRemainsUnchanged(t *testing.T) {
	for _, failure := range []string{"policy", "storage"} {
		t.Run(failure, func(t *testing.T) {
			f := selectionFixture(t)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "execution-read-error", "cli", 2, 1))
			want := "review_conflict"
			if failure == "policy" {
				admissionPolicy(t, f, 1, "cli")
			} else {
				if err := f.store.DB().Close(); err != nil {
					t.Fatal(err)
				}
				want = "storage_unavailable"
			}
			started := time.Now()
			current, err := f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID)
			attemptCode(t, err, want)
			if !reflect.DeepEqual(current, ExecutionContext{}) || time.Since(started) >= time.Second {
				t.Fatal("non-overload error returned partial facts or entered retry backoff", current, err)
			}
		})
	}
}
