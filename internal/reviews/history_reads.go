package reviews

import (
	"context"
	"database/sql"
)

// historyReads reuses repository-owned SQL shapes only within one SQL
// transaction. Values remain parameters; no result or statement outlives it.
type historyReads struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func newHistoryReads(tx *sql.Tx) *historyReads {
	return &historyReads{tx: tx, statements: map[string]*sql.Stmt{}}
}

func (q *historyReads) close() {
	for _, statement := range q.statements {
		_ = statement.Close()
	}
}

func (q *historyReads) statement(ctx context.Context, query string) (*sql.Stmt, error) {
	if statement := q.statements[query]; statement != nil {
		return statement, nil
	}
	statement, err := q.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	q.statements[query] = statement
	return statement, nil
}

func (q *historyReads) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	statement, err := q.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	return statement.QueryContext(ctx, args...)
}

func (q *historyReads) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	statement, err := q.statement(ctx, query)
	if err != nil {
		// sql.Row exposes failures at Scan. Preserve the transaction driver's
		// ordinary error path when preparation failed rather than hiding it.
		return q.tx.QueryRowContext(ctx, query, args...)
	}
	return statement.QueryRowContext(ctx, args...)
}
