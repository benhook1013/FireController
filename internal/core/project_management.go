package core

import "context"

// Project management is an explicit grant alongside read/write, never an
// instance-administrator credential or an automatic upgrade of existing writers.
func (s *Store) ensureProjectManagement(ctx context.Context) error {
	return s.EnsureSchema(ctx, "project_management", []string{
		`ALTER TABLE grants ADD COLUMN can_manage INTEGER NOT NULL DEFAULT 0 CHECK(can_manage IN(0,1) AND (can_manage=0 OR (can_read=1 AND can_write=1)))`,
	})
}
