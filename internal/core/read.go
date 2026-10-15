package core

import (
	"context"
	"database/sql"
	"time"
)

// Read authorizes and reads one consistent database snapshot. Callbacks must
// finish local queries without waiting on clients or external work.
func (s *Store) Read(ctx context.Context, scope Scope, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return storageError(err)
	}
	defer tx.Rollback()
	if err = s.authorize(ctx, tx, scope, "read"); err != nil {
		return err
	}
	err = fn(ctx, tx)
	if ctx.Err() != nil {
		return storageError(ctx.Err())
	}
	if err != nil {
		return mutationError(ctx, err)
	}
	return nil
}
