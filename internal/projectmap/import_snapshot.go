package projectmap

import (
	"context"
	"database/sql"
	"errors"
)

// validateImportSnapshot runs only during operator snapshot inspection. Runtime
// reads and imports never scan all retained source revisions.
func validateImportSnapshot(ctx context.Context, tx *sql.Tx) error {
	var version string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version)
	if err != nil {
		return err
	}
	if version == "1" {
		return nil
	}
	if version != "2" {
		return unavailable()
	}
	var orphans int
	if err = tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM project_map_import_sources s LEFT JOIN projects p ON p.id=s.project_id WHERE p.id IS NULL)+
 (SELECT count(*) FROM project_map_import_history h LEFT JOIN project_map_import_sources s ON s.project_id=h.project_id AND s.source_key=h.source_key WHERE s.source_key IS NULL)+
 (SELECT count(*) FROM project_map_import_items i LEFT JOIN project_map_import_sources s ON s.project_id=i.project_id AND s.source_key=i.source_key WHERE s.source_key IS NULL)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return unavailable()
	}

	// A destination entry is created for one source item, never rebound to a
	// second identity. Check this once across the inventory, not per source.
	var duplicate int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM project_map_import_items GROUP BY project_id,entry_id HAVING count(*)>1 LIMIT 1`).Scan(&duplicate)
	if err == nil {
		return unavailable()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id,source_key FROM project_map_import_sources ORDER BY project_id,source_key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var project, source string
		if err = rows.Scan(&project, &source); err != nil {
			return err
		}
		if !validID(project) || !importKey.MatchString(source) {
			return unavailable()
		}
		if err = validateImportHistory(ctx, tx, project, source); err != nil {
			return err
		}
	}
	return rows.Err()
}
func validateImportHistory(ctx context.Context, tx *sql.Tx, project, source string) error {
	current, err := readImportRecord(ctx, tx, project, source, 0)
	if err != nil {
		return err
	}
	var repository int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,worker_id,created_at,CASE WHEN length(CAST(record_json AS BLOB))<=? THEN record_json ELSE NULL END FROM project_map_import_history WHERE project_id=? AND source_key=? ORDER BY revision`, MaxImportRecordBytes, project, source)
	if err != nil {
		return err
	}
	defer rows.Close()
	latest := map[string]ImportMapping{}
	previousDoc := ImportDocument{}
	var count int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rev int64
		var actor, worker, at, raw string
		if err = rows.Scan(&rev, &actor, &worker, &at, &raw); err != nil {
			return err
		}
		record, err := decodeImportRecord(raw)
		if err != nil {
			return err
		}
		if count == current.Revision || rev != count+1 || record.Revision != rev || record.SourceKey != source || record.ActorID != actor || record.WorkerID != worker || record.CreatedAt != at || record.Document.Source.RepositoryID != repository || count > 0 && sameImportDocument(previousDoc, record.Document) {
			return unavailable()
		}
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM workers WHERE project_id=? AND id=?`, project, worker).Scan(&exists); err != nil {
			return unavailable()
		}
		for n, m := range record.Mappings {
			item := record.Document.Entries[n]
			previous, existed := latest[m.SourceKey]
			if !existed {
				if item.ExpectedRevision != 0 || m.EntryRevision != 1 {
					return unavailable()
				}
			} else {
				if m.EntryID != previous.EntryID || m.EntryRevision < previous.EntryRevision {
					return unavailable()
				}
				if item.ExpectedRevision == 0 {
					if m.EntryRevision != previous.EntryRevision {
						return unavailable()
					}
				} else if item.ExpectedRevision < previous.EntryRevision || m.EntryRevision != item.ExpectedRevision && m.EntryRevision != item.ExpectedRevision+1 {
					return unavailable()
				}
			}
			var captured string
			err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_map_entry_history WHERE project_id=? AND entry_id=? AND revision=?`, maxEntryBytes, project, m.EntryID, m.EntryRevision).Scan(&captured)
			if err != nil {
				return unavailable()
			}
			e, err := decodeEntry(captured)
			if err != nil || e.ProjectID != project || e.ID != m.EntryID || e.Revision != m.EntryRevision || !equalEditable(editable(e), item.Entry) {
				return unavailable()
			}
			latest[m.SourceKey] = m
		}
		previousDoc = record.Document
		count = rev
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if count != current.Revision {
		return unavailable()
	}
	bindings, err := tx.QueryContext(ctx, `SELECT item_key,entry_id,entry_revision,source_revision FROM project_map_import_items WHERE project_id=? AND source_key=? ORDER BY item_key`, project, source)
	if err != nil {
		return err
	}
	defer bindings.Close()
	seen := 0
	for bindings.Next() {
		var m ImportMapping
		if err = bindings.Scan(&m.SourceKey, &m.EntryID, &m.EntryRevision, &m.SourceRevision); err != nil {
			return err
		}
		expected, exists := latest[m.SourceKey]
		if !exists || m != expected {
			return unavailable()
		}
		seen++
	}
	if err = bindings.Err(); err != nil {
		return err
	}
	if seen != len(latest) {
		return unavailable()
	}
	return nil
}
