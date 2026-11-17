package reviewexec

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRoundCompletionSameCallReuseParity(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 2, 4, "same-call-grant")
	a, id := roundCapture(t, f, 0, true)
	// Retained unfinalized evidence must not acquire completion credit.
	check := func(want string, accepted int64) {
		t.Helper()
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		q := newOccupancyQueries(tx)
		defer q.close()
		for _, reader := range []EvidenceQuery{unpreparedSnapshotQuery{q}, q} {
			for _, external := range []bool{false, true} {
				got, n, err := roundCompletionAuthority(t.Context(), reader, attemptProject, a.OperationID, external)
				if err != nil || got != want || n != accepted {
					t.Fatalf("authority parity got=%q n=%d err=%v", got, n, err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := roundCompletionAuthority(ctx, reader, attemptProject, a.OperationID, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation changed: %v", err)
			}
		}
	}
	check("", 0)
	roundFinalize(t, f, a, id, 0, true)
	check(id, 1)
	// Reuse is call-local, not a retained result: later corruption in the same
	// transaction is re-read and rejected by both paths.
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := newOccupancyQueries(tx)
	defer q.close()
	if _, _, err := roundCompletionAuthority(t.Context(), q, attemptProject, a.OperationID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE review_execution_evidence SET body_json='{}' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []EvidenceQuery{unpreparedSnapshotQuery{q}, q} {
		if _, _, err := roundCompletionAuthority(t.Context(), reader, attemptProject, a.OperationID, false); err == nil {
			t.Fatal("corrupt evidence credited")
		}
	}
}

func TestRoundCompletionSameCallReuseCost(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 1, 4, "same-call-cost")
	a, id := roundCapture(t, f, 0, true)
	roundFinalize(t, f, a, id, 0, true)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := newOccupancyQueries(tx)
	defer q.close()
	for pass := 0; pass < 4; pass++ {
		for _, reuse := range []bool{false, true} {
			var reader EvidenceQuery = unpreparedSnapshotQuery{q}
			if reuse {
				reader = q
			}
			start := time.Now()
			for i := 0; i < 100; i++ {
				got, n, err := roundCompletionAuthority(t.Context(), reader, attemptProject, a.OperationID, false)
				if err != nil || got != id || n != 1 {
					t.Fatal("authority result changed", err)
				}
			}
			t.Logf("reuse=%t pass=%d elapsed=%s", reuse, pass, time.Since(start))
		}
	}
}
