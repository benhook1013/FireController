// Package tracking implements project-scoped milestones, work items and typed
// item links. It is generic: callers choose keys, labels and link targets, and
// FireController never reads repository files to populate it.
package tracking

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
)

// Domain is the schema domain recorded in the store and checked by backups.
const Domain = "project_tracking"

// SchemaSQL returns an owned copy of this domain's canonical physical schema.
func SchemaSQL() []string {
	return []string{
		`CREATE TABLE tracking_milestones (project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,key TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('open','closed')),revision INTEGER NOT NULL CHECK(revision>=1),record_json TEXT NOT NULL,PRIMARY KEY(project_id,key))`,
		`CREATE TABLE tracking_milestone_history (project_id TEXT NOT NULL,milestone_key TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),actor_id TEXT NOT NULL,record_json TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,milestone_key,revision),FOREIGN KEY(project_id,milestone_key) REFERENCES tracking_milestones(project_id,key) ON DELETE CASCADE)`,
		`CREATE TABLE tracking_items (project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,key TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('planned','active','blocked','deferred','done','dropped')),milestone_key TEXT,parent_key TEXT,position INTEGER NOT NULL CHECK(position>=0),revision INTEGER NOT NULL CHECK(revision>=1),record_json TEXT NOT NULL,PRIMARY KEY(project_id,key),FOREIGN KEY(project_id,milestone_key) REFERENCES tracking_milestones(project_id,key),FOREIGN KEY(project_id,parent_key) REFERENCES tracking_items(project_id,key))`,
		`CREATE INDEX tracking_items_order ON tracking_items(project_id,position,key)`,
		`CREATE INDEX tracking_items_milestone ON tracking_items(project_id,milestone_key,status)`,
		`CREATE INDEX tracking_items_parent ON tracking_items(project_id,parent_key,status)`,
		`CREATE TABLE tracking_item_history (project_id TEXT NOT NULL,item_key TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),actor_id TEXT NOT NULL,record_json TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,item_key,revision),FOREIGN KEY(project_id,item_key) REFERENCES tracking_items(project_id,key) ON DELETE CASCADE)`,
		`CREATE TABLE tracking_item_links (project_id TEXT NOT NULL,item_key TEXT NOT NULL,target_type TEXT NOT NULL CHECK(target_type IN ('job','pr','route','note','item','url')),target_ref TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('delivers','proof','blocks','related')),PRIMARY KEY(project_id,item_key,target_type,target_ref,role),FOREIGN KEY(project_id,item_key) REFERENCES tracking_items(project_id,key) ON DELETE CASCADE)`,
		`CREATE INDEX tracking_item_links_target ON tracking_item_links(project_id,target_type,target_ref,item_key)`,
	}
}

// TableNames lists the tables SchemaSQL creates, in creation order.
func TableNames() []string {
	return []string{"tracking_milestones", "tracking_milestone_history", "tracking_items", "tracking_item_history", "tracking_item_links"}
}

// EnsureSchema runs during exclusive startup, after the core project schema.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, Domain, SchemaSQL())
}
