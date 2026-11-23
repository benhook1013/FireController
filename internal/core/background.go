package core

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

// BackgroundWrite is the bounded transaction boundary for trusted in-process
// supervisors. It does not authenticate a user or manufacture a user receipt.
// A domain must recheck its retained actor/configuration before external work.
// Callbacks perform local statements only and use the supplied context.
func (s *Store) BackgroundWrite(ctx context.Context, fn func(context.Context, *sql.Tx) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	started := time.Now()
	stage := "begin"
	var err error
	// Emit after rollback and writer release. Public errors remain sanitized,
	// and expected budget or lock contention does not generate another log.
	defer func() {
		var failure *Error
		if !errors.As(resultErr, &failure) || failure.Code != "storage_unavailable" {
			return
		}
		attributes := []slog.Attr{
			slog.String("stage", stage),
			slog.Int64("elapsed_ms", time.Since(started).Milliseconds()),
		}
		var coded interface{ Code() int }
		switch {
		case errors.As(err, &coded):
			code := coded.Code()
			attributes = append(attributes, slog.String("cause", "sqlite"), slog.Int("sqlite_code", code), slog.Int("sqlite_primary_code", code&255))
		case errors.Is(err, sql.ErrTxDone):
			attributes = append(attributes, slog.String("cause", "transaction_done"))
		default:
			attributes = append(attributes, slog.String("cause", "unknown"))
		}
		slog.LogAttrs(ctx, slog.LevelWarn, "background_write_failed", attributes...)
	}()
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
	stage = "apply"
	if err = fn(ctx, tx); err != nil {
		return mutationError(ctx, err)
	}
	if ctx.Err() != nil {
		return storageError(ctx.Err())
	}
	stage = "commit"
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
