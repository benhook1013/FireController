package reviewexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRunSelectionSourceBudgetRetainsRemainingTimeAcrossLocalWork(t *testing.T) {
	budget := runSourceBudget{remaining: time.Second}
	if err := budget.read(t.Context(), func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("source read lacks its finite budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remaining := budget.remaining
	if remaining <= 0 || remaining >= time.Second {
		t.Fatal("first source read did not consume cumulative time", remaining)
	}
	// No source callback or context is retained during local admission/backoff.
	// Waiting longer than the remaining budget outside read must not spend it.
	time.Sleep(remaining + 10*time.Millisecond)
	if budget.remaining != remaining {
		t.Fatal("local work spent source budget")
	}
	if err := budget.read(t.Context(), func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > remaining || ctx.Err() != nil {
			t.Fatal("second source read reset or expired its retained budget")
		}
		return nil
	}); err != nil || budget.remaining >= remaining {
		t.Fatal("second source read lost cumulative accounting", err)
	}
}

func TestRunSelectionSourceBudgetRefusesLateSuccessAndFurtherReads(t *testing.T) {
	budget := runSourceBudget{remaining: 20 * time.Millisecond}
	err := budget.read(t.Context(), func(ctx context.Context) error {
		<-ctx.Done()
		return nil // A provider ignoring its deadline must not supply success.
	})
	if !errors.Is(err, context.DeadlineExceeded) || budget.remaining > 0 || t.Context().Err() != nil {
		t.Fatal("source timeout was lost or canceled the worker", err, budget.remaining)
	}
	called := false
	err = budget.read(t.Context(), func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("exhausted total source budget allowed another read", err, called)
	}
}

func TestRunSelectionSourceBudgetPreservesCancellationAndReadFailure(t *testing.T) {
	budget := runSourceBudget{remaining: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	if err := budget.read(ctx, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled worker started a source read", err, called)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	if err := budget.read(ctx, func(source context.Context) error {
		cancel()
		<-source.Done()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("source read erased worker cancellation", err)
	}
	failure := errors.New("synthetic source failure")
	if err := budget.read(t.Context(), func(context.Context) error { return failure }); err != failure {
		t.Fatal("source read failure changed", err)
	}
}

func TestRunSelectionSourceTimeoutStillProducesSourceHold(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "source-timeout", "cli", nil)
	reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "source_fetch_timeout" || got.OperationID != "" || reader.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("source timeout lost its protective hold", got, reader.calls.Load())
	}
}
