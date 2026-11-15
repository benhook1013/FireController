package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

type unpreparedSnapshotQuery struct{ EvidenceQuery }

func TestSnapshotQueriesSelectionParityAndCost(t *testing.T) {
	f, s := selectionSnapshotFixture(t)
	for i := 0; i < 66; i++ {
		selectionSnapshotEnqueue(t, f, s, fmt.Sprintf("prepared-snapshot-%d", i))
		if _, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{merged: true}}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for pass := 0; pass < 3; pass++ {
		for _, prepared := range []bool{false, true} {
			var q EvidenceQuery = unpreparedSnapshotQuery{tx}
			if prepared {
				q = tx
			}
			start := time.Now()
			if err := ValidateSelectionSnapshot(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			t.Logf("prepared=%t pass=%d validation=%s", prepared, pass, time.Since(start))
		}
	}
	// Check cancellation before corrupting this otherwise valid snapshot.
	for _, q := range []EvidenceQuery{tx, unpreparedSnapshotQuery{tx}} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := ValidateSelectionSnapshot(ctx, q); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation on valid snapshot, got %v", err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE review_selections SET worker_id='missing-worker' WHERE id=(SELECT id FROM review_selections ORDER BY id LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []EvidenceQuery{tx, unpreparedSnapshotQuery{tx}} {
		if err := ValidateSelectionSnapshot(t.Context(), q); err == nil {
			t.Fatal("corruption accepted")
		}
	}
}

func TestSnapshotQueriesOnlyExistingTransaction(t *testing.T) {
	f := roundFixture(t)
	q, closeQueries := snapshotQueries(f.store.DB())
	defer closeQueries()
	if q != f.store.DB() {
		t.Fatal("changed nontransaction reader")
	}
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		q, closeQueries := snapshotQueries(tx)
		p, ok := q.(*occupancyQueries)
		if !ok || p.tx != tx {
			t.Fatal("lost caller transaction")
		}
		var v int
		if err := q.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
			return err
		}
		stmt := p.statements["SELECT 1"]
		closeQueries()
		if err := stmt.QueryRowContext(ctx).Scan(&v); err == nil {
			t.Fatal("program survived validation")
		}
		if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil {
			return err
		}
		return nil
	})
}
