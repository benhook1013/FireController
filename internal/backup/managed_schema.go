package backup

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureManagedSchema installs only local operator scheduling facts. These rows
// neither authorize provider work nor participate in worker events or receipts.
func EnsureManagedSchema(ctx context.Context, store *core.Store) error {
	if ctx == nil || store == nil {
		return unavailable()
	}
	if err := store.EnsureSchema(ctx, "managed_backups", []string{
		`CREATE TABLE managed_backup_state(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body_json TEXT NOT NULL)`,
		`CREATE TABLE managed_backup_bundles(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,destination TEXT NOT NULL,source_instance_id TEXT NOT NULL,manifest_sha256 TEXT NOT NULL,created_at TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('retained','deleting','cleanup_failed','deleted')))`,
		`CREATE INDEX managed_backup_retention ON managed_backup_bundles(destination,source_instance_id,state,sequence DESC)`,
	}); err != nil {
		return err
	}
	return store.EnsureSchema(ctx, "managed_backup_publications", []string{
		`CREATE TABLE managed_backup_publication_holds(bundle_id TEXT PRIMARY KEY,destination TEXT NOT NULL,source_instance_id TEXT NOT NULL,manifest_sha256 TEXT NOT NULL,summary_json TEXT NOT NULL)`,
		`CREATE INDEX managed_backup_publication_destination ON managed_backup_publication_holds(destination,source_instance_id)`,
	})
}
