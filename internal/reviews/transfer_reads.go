package reviews

import (
	"context"
	"database/sql"
)

// A bounded batch repeats a few repository-owned SELECT shapes for each record.
// Keep their prepared statements only for this authorized transaction; values
// remain parameters and neither statements nor results survive the transaction.
type transferReads struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func newTransferReads(tx *sql.Tx) *transferReads {
	return &transferReads{tx: tx, statements: map[string]*sql.Stmt{}}
}
func (q *transferReads) close() {
	for _, stmt := range q.statements {
		_ = stmt.Close()
	}
}
func (q *transferReads) prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt := q.statements[query]; stmt != nil {
		return stmt, nil
	}
	stmt, err := q.tx.PrepareContext(ctx, query)
	if err == nil {
		q.statements[query] = stmt
	}
	return stmt, err
}
func (q *transferReads) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	stmt, err := q.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	return stmt.QueryContext(ctx, args...)
}
func (q *transferReads) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	stmt, err := q.prepare(ctx, query)
	if err != nil {
		// database/sql exposes row errors through Scan, not a Row constructor.
		// Preserve its ordinary error path if preparation itself failed.
		return q.tx.QueryRowContext(ctx, query, args...)
	}
	return stmt.QueryRowContext(ctx, args...)
}
