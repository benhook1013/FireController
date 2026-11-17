package reviewexec

import (
	"context"
	"database/sql"
)

// occupancyQueries reuses SQL programs, never results, during one synchronous
// occupancy scan. Every read still uses the caller's transaction and context;
// canonical evidence, scope, and corruption checks are unchanged. Statements
// cannot survive this scan or be shared with another transaction/goroutine.
type occupancyQueries struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func newOccupancyQueries(tx *sql.Tx) *occupancyQueries {
	return &occupancyQueries{tx: tx, statements: make(map[string]*sql.Stmt)}
}

func (q *occupancyQueries) statement(ctx context.Context, query string) (*sql.Stmt, error) {
	if s := q.statements[query]; s != nil {
		return s, nil
	}
	// Bound retained SQL programs even if a future reader varies query text.
	if len(q.statements) >= 32 {
		return nil, nil
	}
	s, err := q.tx.PrepareContext(ctx, query)
	if err == nil {
		q.statements[query] = s
	}
	return s, err
}

func (q *occupancyQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s, err := q.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return q.tx.QueryContext(ctx, query, args...)
	}
	return s.QueryContext(ctx, args...)
}

func (q *occupancyQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s, err := q.statement(ctx, query)
	if err != nil || s == nil {
		// sql.Row has no public error constructor. Execute the identical read on
		// the identical transaction/context, preserving Scan's normal error path.
		return q.tx.QueryRowContext(ctx, query, args...)
	}
	return s.QueryRowContext(ctx, args...)
}

func (q *occupancyQueries) close() {
	for _, s := range q.statements {
		_ = s.Close()
	}
}
