package core

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type readAdmissionOverload struct{ error }

func (e *readAdmissionOverload) Unwrap() error { return e.error }

// IsReadAdmissionOverload identifies a Read transaction-admission deadline while
// its caller is still live. Authorization, query, and caller failures do not match.
func IsReadAdmissionOverload(err error) bool {
	var admission *readAdmissionOverload
	return errors.As(err, &admission)
}

// Read authorizes and reads one consistent database snapshot. Callbacks must
// finish local queries without waiting on clients or external work.
func (s *Store) Read(ctx context.Context, scope Scope, fn func(context.Context, *sql.Tx) error) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
			return &readAdmissionOverload{storageError(err)}
		}
		return storageError(err)
	}
	defer tx.Rollback()
	permission := "read"
	if scope.admin {
		permission = "admin"
	}
	if err = s.authorize(ctx, tx, scope, permission); err != nil {
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
