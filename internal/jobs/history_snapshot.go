package jobs

import (
	"context"
	"database/sql"
)

type historySnapshotReader interface {
	queryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ValidateHistorySnapshot inspects every retained row without requiring that
// all intermediate revisions survived compaction. First and current revisions
// must remain; retained snapshots use the same validation as the history API.
func ValidateHistorySnapshot(ctx context.Context, q historySnapshotReader) error {
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs j WHERE
        j.revision != COALESCE((SELECT MAX(h.revision) FROM job_history h WHERE h.project_id=j.project_id AND h.job_id=j.id),0)
        OR NOT EXISTS(SELECT 1 FROM job_history h WHERE h.project_id=j.project_id AND h.job_id=j.id AND h.revision=1))`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return jobHistoryUnavailable()
	}
	rows, err := q.QueryContext(ctx, `SELECT project_id,job_id,revision,actor_id,
        CASE WHEN typeof(state_json)='text' AND length(CAST(state_json AS BLOB))<=? THEN state_json END,
        created_at FROM job_history ORDER BY project_id,job_id,revision`, maxJobContext)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project, id string
		var item historyEntry
		var state sql.NullString
		if err := rows.Scan(&project, &id, &item.Revision, &item.ActorID, &state, &item.CreatedAt); err != nil {
			return err
		}
		item.State = []byte(state.String)
		if !state.Valid || !validStoredJobHistory(item, project, id) {
			return jobHistoryUnavailable()
		}
	}
	return rows.Err()
}
