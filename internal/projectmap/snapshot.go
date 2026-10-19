package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
)

// ValidateSnapshot checks retained logical bindings during operator backup and
// inspection. Ordinary map reads never scan archives. The caller owns a single
// read transaction and schema/foreign-key validation.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM project_map_entries ORDER BY project_id,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var project, id string
		if err = rows.Scan(&project, &id); err != nil {
			return err
		}
		current, err := readEntry(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if err = validateEntryHistory(ctx, tx, current); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	// Include explicit orphan checks so this helper also fails closed when its
	// caller has not enabled SQLite's foreign-key enforcement on that connection.
	var orphans int
	err = tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM project_map_entries e LEFT JOIN projects p ON p.id=e.project_id WHERE p.id IS NULL)+
 (SELECT count(*) FROM project_map_entry_history h LEFT JOIN project_map_entries e ON e.project_id=h.project_id AND e.id=h.entry_id WHERE e.id IS NULL)+
 (SELECT count(*) FROM job_workstreams a LEFT JOIN jobs j ON j.project_id=a.project_id AND j.id=a.job_id WHERE j.id IS NULL)+
 (SELECT count(*) FROM job_workstream_history h LEFT JOIN job_workstreams a ON a.project_id=h.project_id AND a.job_id=h.job_id WHERE a.job_id IS NULL)`).Scan(&orphans)
	if err != nil {
		return err
	}
	if orphans != 0 {
		return unavailable()
	}
	associations, err := tx.QueryContext(ctx, `SELECT project_id,job_id FROM job_workstreams ORDER BY project_id,job_id`)
	if err != nil {
		return err
	}
	defer associations.Close()
	for associations.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var project, job string
		if err = associations.Scan(&project, &job); err != nil {
			return err
		}
		if !validID(project) || !validID(job) {
			return unavailable()
		}
		if err = validateJobHistory(ctx, tx, project, job); err != nil {
			return err
		}
	}
	if err := associations.Err(); err != nil {
		return err
	}
	if err := associations.Close(); err != nil {
		return err
	}
	return validateImportSnapshot(ctx, tx)
}
func validateEntryHistory(ctx context.Context, tx *sql.Tx, current Entry) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END,created_at FROM project_map_entry_history WHERE project_id=? AND entry_id=? ORDER BY revision`, maxEntryBytes, current.ProjectID, current.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var previous Entry
	var count int64
	for rows.Next() {
		var rev int64
		var actor, at string
		var raw sql.NullString
		if err = rows.Scan(&rev, &actor, &raw, &at); err != nil {
			return err
		}
		if !raw.Valid {
			return unavailable()
		}
		value, err := decodeEntry(raw.String)
		if err != nil {
			return err
		}
		if count == current.Revision || rev != count+1 || value.Revision != rev || value.ProjectID != current.ProjectID || value.ID != current.ID || value.Kind != current.Kind || value.CreatedAt != current.CreatedAt || value.UpdatedAt != at || !validID(actor) {
			return unavailable()
		}
		if rev == 1 && value.CreatedAt != at || rev > 1 && (!retainsPhases(editable(previous), editable(value)) || equalEditable(editable(previous), editable(value))) {
			return unavailable()
		}
		previous = value
		count = rev
	}
	if err = rows.Err(); err != nil {
		return err
	}
	a, _ := json.Marshal(previous)
	b, _ := json.Marshal(current)
	if count != current.Revision || string(a) != string(b) {
		return unavailable()
	}
	return nil
}
func validateJobHistory(ctx context.Context, tx *sql.Tx, project, job string) error {
	current, err := readAssociation(ctx, tx, project, job)
	if err != nil {
		return err
	}
	jobRev, err := jobRevision(ctx, tx, project, job)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,COALESCE(workstream_id,''),workstream_title,job_revision,actor_id,created_at FROM job_workstream_history WHERE project_id=? AND job_id=? ORDER BY revision`, project, job)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int64
	var last JobWorkstreamHistory
	for rows.Next() {
		var h JobWorkstreamHistory
		if err = rows.Scan(&h.Revision, &h.WorkstreamID, &h.WorkstreamTitle, &h.JobRevision, &h.ActorID, &h.CreatedAt); err != nil {
			return err
		}
		if count == current.Revision || h.Revision != count+1 || h.JobRevision < 1 || h.JobRevision > jobRev || h.JobRevision < last.JobRevision || !validAssociation(h.WorkstreamID, h.WorkstreamTitle) || !validID(h.ActorID) || !validStamp(h.CreatedAt) {
			return unavailable()
		}
		if h.WorkstreamID != "" {
			e, err := readEntry(ctx, tx, project, h.WorkstreamID)
			if err != nil {
				return unavailable()
			}
			if e.Kind != "workstream" {
				return unavailable()
			}
		}
		if h.Revision == 1 && h.WorkstreamID == "" || h.Revision > 1 && last.WorkstreamID == h.WorkstreamID {
			return unavailable()
		}
		last = h
		count = h.Revision
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != current.Revision || last.WorkstreamID != current.WorkstreamID || last.WorkstreamTitle != current.WorkstreamTitle {
		return unavailable()
	}
	return nil
}
