package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestOccupancyQueriesRetainCurrentTransactionAndCancellation(t *testing.T) {
	f := roundFixture(t)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		q := newOccupancyQueries(tx)
		defer q.close()
		const query = `SELECT value FROM metadata WHERE key='schema:review_execution'`
		var before, after string
		if err := q.QueryRowContext(ctx, query).Scan(&before); err != nil {
			return err
		}
		first := q.statements[query]
		if first == nil {
			return fmt.Errorf("query was not prepared")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value='changed' WHERE key='schema:review_execution'`); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, query).Scan(&after); err != nil {
			return err
		}
		if after != "changed" || q.statements[query] != first {
			return fmt.Errorf("statement reuse cached evidence or lost prepared identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value=? WHERE key='schema:review_execution'`, before); err != nil {
			return err
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := q.QueryRowContext(cancelled, query).Scan(&after); err == nil {
			return fmt.Errorf("cached statement ignored cancellation")
		}
		if rows, err := q.QueryContext(cancelled, query); err == nil {
			rows.Close()
			return fmt.Errorf("cached rows ignored cancellation")
		}
		if err := q.QueryRowContext(ctx, `SELECT absent_occupancy_column FROM metadata`).Scan(&after); err == nil {
			return fmt.Errorf("prepare fallback hid SQL error")
		}
		q.close()
		if err := first.QueryRowContext(ctx).Scan(&after); err == nil {
			return fmt.Errorf("statement survived scan cleanup")
		}
		return nil
	})
}

func TestOccupancyQueriesBoundProgramsAndSeparateTransactions(t *testing.T) {
	f := roundFixture(t)
	var previous *sql.Stmt
	for pass := 0; pass < 2; pass++ {
		roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
			q := newOccupancyQueries(tx)
			defer q.close()
			for i := 0; i < 40; i++ {
				var got int
				if err := q.QueryRowContext(ctx, fmt.Sprintf("SELECT %d", i)).Scan(&got); err != nil || got != i {
					return fmt.Errorf("read %d: %d %v", i, got, err)
				}
			}
			if len(q.statements) != 32 {
				return fmt.Errorf("program bound=%d", len(q.statements))
			}
			if q.statements["SELECT 0"] == previous {
				return fmt.Errorf("statement crossed transaction")
			}
			previous = q.statements["SELECT 0"]
			return nil
		})
	}
}
