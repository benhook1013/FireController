package core

import "context"

// EnsureSchema is for fixed domain DDL at exclusive startup, before serving.
// It does not accept client input or run live migrations.
func (s *Store) EnsureSchema(ctx context.Context, statements ...string) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
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
	if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return storageError(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	for _, statement := range statements {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return storageError(err)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return storageError(err)
	}
	committed = true
	return nil
}
