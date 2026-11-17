package core

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestMutationContentionLeavesFullBoundedTransactionBudget(t *testing.T) {
	s, scope, _ := streamFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	backgroundDone := make(chan error, 1)
	go func() {
		backgroundDone <- s.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('contention-owner','saved')`); err != nil {
				return err
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	mutation := Mutation{Operation: "contention.save", Target: "probe", Key: "retained-contention-key"}
	calls := 0
	apply := func(ctx context.Context, tx *sql.Tx) (any, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 1800*time.Millisecond || time.Until(deadline) > writeTimeout {
			return nil, errors.New("queue waiting consumed the bounded transaction budget")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('contention-result','saved')`); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, tx, scope, "contention.saved", "probe", 1, struct{}{}); err != nil {
			return nil, err
		}
		// Model finite local transaction work after a separate finite writer
		// wait. Neither phase may consume the other's execution allowance.
		select {
		case <-time.After(1200 * time.Millisecond):
			return map[string]string{"outcome": "saved"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	started := time.Now()
	go func() {
		body, err := s.Mutate(t.Context(), scope, mutation, apply)
		done <- result{body, err}
	}()
	time.Sleep(1200 * time.Millisecond)
	close(release)
	if err := <-backgroundDone; err != nil {
		t.Fatal("background owner failed", err)
	}
	first := <-done
	if first.err != nil || len(first.body) == 0 || time.Since(started) > 4*time.Second {
		t.Fatal("finite writer contention prevented ordinary mutation", first.err, time.Since(started))
	}
	replayed, err := s.Mutate(t.Context(), scope, mutation, apply)
	if err != nil || !bytes.Equal(first.body, replayed) || calls != 1 {
		t.Fatal("contention recovery duplicated a mutation", string(replayed), calls, err)
	}
	var committed int
	if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM metadata WHERE key IN ('contention-owner','contention-result'))+(SELECT count(*) FROM receipts)+(SELECT count(*) FROM events)`).Scan(&committed); err != nil || committed != 4 {
		t.Fatal("contention mutation lost atomic data/event/receipt", committed, err)
	}
}

func TestMutationContentionPreservesParentDeadlineAndCancellation(t *testing.T) {
	for _, stop := range []string{"expired", "cancelled-admission", "deadline-after-admission", "cancelled-apply"} {
		t.Run(stop, func(t *testing.T) {
			s, scope, _ := streamFixture(t)
			var ctx context.Context
			var cancel context.CancelFunc
			if stop == "expired" {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
			}
			defer cancel()
			if stop == "cancelled-admission" || stop == "deadline-after-admission" {
				s.admission <- struct{}{}
			}
			entered := make(chan struct{})
			var calls int
			done := make(chan error, 1)
			go func() {
				_, err := s.Mutate(ctx, scope, Mutation{Operation: "parent.save", Target: "probe", Key: "parent-bound"}, func(work context.Context, tx *sql.Tx) (any, error) {
					calls++
					parentDeadline, _ := ctx.Deadline()
					workDeadline, ok := work.Deadline()
					if !ok || workDeadline.After(parentDeadline) {
						return nil, errors.New("transaction extended its parent deadline")
					}
					if _, err := tx.ExecContext(work, `INSERT INTO metadata(key,value) VALUES('parent-result','must-roll-back')`); err != nil {
						return nil, err
					}
					close(entered)
					<-work.Done()
					return nil, work.Err()
				})
				done <- err
			}()
			switch stop {
			case "cancelled-admission":
				cancel()
			case "deadline-after-admission":
				time.Sleep(250 * time.Millisecond)
				<-s.admission
			case "cancelled-apply":
				<-entered
				cancel()
			}
			err := <-done
			if stop == "cancelled-admission" {
				<-s.admission
			}
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" {
				t.Fatal("parent boundary lost its classified refusal", err)
			}
			if (stop == "expired" || stop == "cancelled-admission") && calls != 0 || (stop == "deadline-after-admission" || stop == "cancelled-apply") && calls != 1 {
				t.Fatal("parent cancellation ran or skipped wrong transaction phase", stop, calls)
			}
			var changes int
			if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM metadata WHERE key='parent-result')+(SELECT count(*) FROM receipts)`).Scan(&changes); err != nil || changes != 0 {
				t.Fatal("parent interruption committed partial effects", changes, err)
			}
		})
	}
}

func TestMutationAdmissionRemainsBoundedWithoutExecutingCallback(t *testing.T) {
	s, scope, _ := streamFixture(t)
	s.admission <- struct{}{}
	called := false
	started := time.Now()
	_, err := s.Mutate(t.Context(), scope, Mutation{Operation: "admission.save", Target: "probe", Key: "bounded-admission"}, func(context.Context, *sql.Tx) (any, error) {
		called = true
		return struct{}{}, nil
	})
	<-s.admission
	var observation *mutationOverload
	if !errors.As(err, &observation) || observation.stage != "writer_admission" || observation.cause != "deadline" || called || time.Since(started) > 3*time.Second {
		t.Fatal("writer admission was unbounded or entered a transaction", called, time.Since(started), err)
	}
}

func TestMutationTransactionDeadlineRollsBackAndRetainsSameKeyRecovery(t *testing.T) {
	s, scope, _ := streamFixture(t)
	mutation := Mutation{Operation: "bounded.apply", Target: "probe", Key: "bounded-apply-key"}
	calls := 0
	apply := func(ctx context.Context, tx *sql.Tx) (any, error) {
		calls++
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('bounded-result','saved')`); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, tx, scope, "bounded.saved", "probe", 1, struct{}{}); err != nil {
			return nil, err
		}
		return map[string]string{"outcome": "saved"}, nil
	}
	_, err := s.Mutate(t.Context(), scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
		value, err := apply(ctx, tx)
		if err != nil {
			return nil, err
		}
		<-ctx.Done()
		return value, ctx.Err()
	})
	var observation *mutationOverload
	if !errors.As(err, &observation) || observation.stage != "apply" || observation.cause != "deadline" {
		t.Fatal("transaction deadline changed", err)
	}
	var committed int
	if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM metadata WHERE key='bounded-result')+(SELECT count(*) FROM receipts)+(SELECT count(*) FROM events)`).Scan(&committed); err != nil || committed != 0 {
		t.Fatal("timed out transaction partially committed", committed, err)
	}
	first, err := s.Mutate(t.Context(), scope, mutation, apply)
	if err != nil {
		t.Fatal("same-key recovery failed", err)
	}
	replayed, err := s.Mutate(t.Context(), scope, mutation, apply)
	if err != nil || !bytes.Equal(first, replayed) || calls != 2 {
		t.Fatal("recovery/replay duplicated effect", calls, err)
	}
}
