package reviews

// SnapshotReferenceIndexSchemaSQL returns the canonical retention index DDL
// for startup and backup validation. Both key columns let retention and
// SQLite check a snapshot reference without scanning project history.
// The workspace has at most one row per project and already uses its primary key.
func SnapshotReferenceIndexSchemaSQL() []string {
	return []string{
		`CREATE INDEX review_pull_versions_snapshot ON review_pull_versions(project_id,version_revision)`,
		`CREATE INDEX review_run_versions_snapshot ON review_run_versions(project_id,version_revision)`,
		`CREATE INDEX review_attempt_versions_snapshot ON review_attempt_versions(project_id,version_revision)`,
		`CREATE INDEX review_retired_versions_snapshot ON review_retired_versions(project_id,version_revision)`,
		`CREATE INDEX review_pull_memberships_snapshot_start ON review_pull_memberships(project_id,start_revision)`,
		`CREATE INDEX review_run_memberships_snapshot_start ON review_run_memberships(project_id,start_revision)`,
		`CREATE INDEX review_attempt_memberships_snapshot_start ON review_attempt_memberships(project_id,start_revision)`,
		`CREATE INDEX review_retired_memberships_snapshot_start ON review_retired_memberships(project_id,start_revision)`,
		`CREATE INDEX review_pull_memberships_snapshot_end ON review_pull_memberships(project_id,end_revision)`,
		`CREATE INDEX review_run_memberships_snapshot_end ON review_run_memberships(project_id,end_revision)`,
		`CREATE INDEX review_attempt_memberships_snapshot_end ON review_attempt_memberships(project_id,end_revision)`,
		`CREATE INDEX review_retired_memberships_snapshot_end ON review_retired_memberships(project_id,end_revision)`,
	}
}
