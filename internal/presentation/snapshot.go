package presentation

import (
	"context"
	"database/sql"
	"encoding/json"
)

// ValidateSnapshot checks all retained presentation bindings in the caller's
// read transaction. It performs no migrations and makes no wall-clock claims.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id FROM project_presentations ORDER BY project_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var project string
		if err = rows.Scan(&project); err != nil {
			return err
		}
		current, err := readCurrent(ctx, tx, project)
		if err != nil {
			return err
		}
		if current.Revision < 1 {
			return unavailable()
		}
		if err = validateWorkerOrder(ctx, tx, project, current.WorkerOrder); err != nil {
			return err
		}
		if err = validateHistory(ctx, tx, current); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var orphans int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM project_presentations c LEFT JOIN projects p ON p.id=c.project_id WHERE p.id IS NULL)+(SELECT count(*) FROM project_presentation_history h LEFT JOIN project_presentations c ON c.project_id=h.project_id WHERE c.project_id IS NULL)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return unavailable()
	}
	return nil
}
func validateHistory(ctx context.Context, tx *sql.Tx, current State) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,created_at,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_presentation_history WHERE project_id=? ORDER BY revision`, maxStateBytes, current.ProjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int64
	var previous State
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var rev int64
		var actor, at, raw string
		if err = rows.Scan(&rev, &actor, &at, &raw); err != nil {
			return err
		}
		s, err := decodeState(raw)
		if err != nil {
			return err
		}
		if count == current.Revision || rev != count+1 || s.Revision != rev || s.ProjectID != current.ProjectID || !validID(actor) || at != s.UpdatedAt || rev > 1 && equalConfig(previous.Config(), s.Config()) {
			return unavailable()
		}
		previous = s
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
