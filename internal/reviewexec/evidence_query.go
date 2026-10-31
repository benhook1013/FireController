package reviewexec

import (
	"context"
	"database/sql"
)

// EvidenceQuery supplies read-only queries on the caller's connection or
// transaction. The caller retains transaction, authorization and deadline
// ownership; evidence validation never opens an independent connection.
type EvidenceQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var (
	_ EvidenceQuery = (*sql.Tx)(nil)
	_ EvidenceQuery = (*sql.Conn)(nil)
	_ EvidenceQuery = (*sql.DB)(nil)
)
