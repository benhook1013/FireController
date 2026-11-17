package core

import (
	"context"
	"database/sql"
)

// BackgroundWrite is the bounded transaction boundary for trusted in-process
// supervisors. It does not authenticate a user or manufacture a user receipt.
// A domain must recheck its retained actor/configuration before external work.
// Callbacks perform local statements only and use the supplied context.
func (s *Store) BackgroundWrite(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	changes := &eventChanges{topics: make(map[messageTopic]struct{})}
	ctx = context.WithValue(ctx, eventChangesKey{}, changes)
	select {
	case s.admission <- struct{}{}:
		defer func() { <-s.admission }()
	case <-ctx.Done():
		return storageError(ctx.Err())
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return mutationError(ctx, err)
	}
	defer tx.Rollback()
	if err = fn(ctx, tx); err != nil {
		return mutationError(ctx, err)
	}
	if ctx.Err() != nil {
		return storageError(ctx.Err())
	}
	if err = tx.Commit(); err != nil {
		return mutationError(ctx, err)
	}
	s.notifyEvents(changes)
	return nil
}

func mutationErrorOrNil(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return mutationError(ctx, err)
}
