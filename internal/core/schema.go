package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Startup maintenance can rewrite retained domain history before serving. Its
// budget is separate from the short admission/transaction budget for live work.
const schemaMigrationTimeout = 30 * time.Second

// EnsureSchema applies ordered, repository-owned domain migrations at exclusive
// startup, before serving. Each statement group advances one schema version.
// Previously unversioned domains begin at version zero. Never use client input.
func (s *Store) EnsureSchema(ctx context.Context, domain string, migrations ...[]string) error {
	return s.EnsureSchemaWithBackfill(ctx, domain, migrations, nil)
}

// EnsureSchemaWithBackfill applies the same exclusive startup migrations as
// EnsureSchema, then runs backfill on that connection before committing. The
// callback runs only when at least one migration is required and shares the
// migration deadline and atomic rollback, including schema version metadata.
// It must perform only bounded database work, with no filesystem, network or
// provider calls, and must not commit or roll back the connection itself.
func (s *Store) EnsureSchemaWithBackfill(ctx context.Context, domain string, migrations [][]string, backfill func(context.Context, *sql.Conn) error) error {
	if domain == "" || len(migrations) == 0 {
		return Fail("schema_invalid", "A domain schema requires an identity and migrations.", 500)
	}
	ctx, cancel := context.WithTimeout(ctx, schemaMigrationTimeout)
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
	key := "schema:" + domain
	var raw string
	version := 0
	err = conn.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&raw)
	if err == nil {
		version, err = strconv.Atoi(raw)
		if err != nil || version < 1 || strconv.Itoa(version) != raw {
			return Fail("schema_invalid", "Domain schema version metadata is invalid.", 500)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return storageError(err)
	}
	if version > len(migrations) {
		return Fail("schema_unsupported", "A domain schema is newer than this service; use a compatible binary.", 500)
	}
	needsBackfill := version < len(migrations)
	for _, migration := range migrations[version:] {
		for _, statement := range migration {
			if _, err = conn.ExecContext(ctx, statement); err != nil {
				return storageError(err)
			}
		}
		version++
		if _, err = conn.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, strconv.Itoa(version)); err != nil {
			return storageError(err)
		}
	}
	if needsBackfill && backfill != nil {
		if err = backfill(ctx, conn); err != nil {
			return storageError(err)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return storageError(err)
	}
	committed = true
	return nil
}
