package reviewexec

import (
	"context"
	"database/sql"
)

type enrollmentExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type enrollmentQuery interface {
	EvidenceQuery
	enrollmentExecer
}

// Enrollment reuses a bounded set of SQL programs within its original mutation.
// No rows, authority, grants or results are cached, and every execution retains
// the caller's context. All programs close before the mutation returns.
type enrollmentQueries struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func newEnrollmentQueries(tx *sql.Tx) *enrollmentQueries {
	return &enrollmentQueries{tx: tx, statements: make(map[string]*sql.Stmt)}
}
func (q *enrollmentQueries) statement(ctx context.Context, query string) (*sql.Stmt, error) {
	if s := q.statements[query]; s != nil {
		return s, nil
	}
	if len(q.statements) >= 8 {
		return nil, nil
	}
	s, err := q.tx.PrepareContext(ctx, query)
	if err == nil {
		q.statements[query] = s
	}
	return s, err
}
func (q *enrollmentQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s, err := q.statement(ctx, query)
	if err != nil || s == nil {
		return q.tx.QueryRowContext(ctx, query, args...)
	}
	return s.QueryRowContext(ctx, args...)
}
func (q *enrollmentQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s, err := q.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return q.tx.QueryContext(ctx, query, args...)
	}
	return s.QueryContext(ctx, args...)
}
func (q *enrollmentQueries) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s, err := q.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return q.tx.ExecContext(ctx, query, args...)
	}
	return s.ExecContext(ctx, args...)
}
func (q *enrollmentQueries) close() {
	for _, s := range q.statements {
		_ = s.Close()
	}
}
