package core

import (
	"context"
	"fmt"
)

// EnsureIncrementalVacuum converts the database once before service startup.
// VACUUM must run outside a transaction, on the same exclusive writer
// connection as the setting; callers must not run it on a serving store.
func (s *Store) EnsureIncrementalVacuum(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.schemaMigrationTimeout)
	defer cancel()
	select {
	case s.admission <- struct{}{}:
		defer func() { <-s.admission }()
	case <-ctx.Done():
		return storageError(ctx.Err())
	}
	conn, err := s.writer.Conn(ctx)
	if err != nil {
		return storageError(err)
	}
	defer conn.Close()
	var mode int
	if err = conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return storageError(err)
	}
	if mode == 2 {
		return nil
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return storageError(err)
	}
	if _, err = conn.ExecContext(ctx, "VACUUM"); err != nil {
		return storageError(err)
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return storageError(err)
	}
	if mode != 2 {
		return storageError(fmt.Errorf("incremental auto_vacuum mode was not persisted: %d", mode))
	}
	// Migration readers may have cached the old database-header mode.
	// Startup has no serving readers; retire idle handles before serving.
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxIdleConns(2)
	return nil
}
