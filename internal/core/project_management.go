package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Project management is an explicit grant alongside read/write, never an
// instance-administrator credential or an automatic upgrade of existing writers.
func (s *Store) ensureProjectManagement(ctx context.Context) error {
	return s.EnsureSchema(ctx, "project_management", []string{
		`ALTER TABLE grants ADD COLUMN can_manage INTEGER NOT NULL DEFAULT 0 CHECK(can_manage IN(0,1) AND (can_manage=0 OR (can_read=1 AND can_write=1)))`,
	})
}

// Check both additive core formats before changing either; partial upgrades must
// not modify a database whose other administration domain is newer.
func (s *Store) checkAdministrationVersions(ctx context.Context) error {
	for _, domain := range []string{"project_management", "worker_retirements"} {
		var raw string
		err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", "schema:"+domain).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return storageError(err)
		}
		version, err := strconv.Atoi(raw)
		if err != nil || version < 1 || strconv.Itoa(version) != raw {
			return Fail("schema_invalid", "Domain schema version metadata is invalid.", 500)
		}
		if version > 1 {
			return Fail("schema_unsupported", "A domain schema is newer than this service; use a compatible binary.", 500)
		}
	}
	return nil
}
