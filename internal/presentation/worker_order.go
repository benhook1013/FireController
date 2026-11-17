package presentation

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// WorkerOrderJSON reads the optional presentation module inside the caller's
// snapshot. Core-only installations retain the same alphabetical default.
func WorkerOrderJSON(ctx context.Context, tx *sql.Tx, project string) (string, error) {
	var installed int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='project_presentations')`).Scan(&installed); err != nil {
		return "", err
	}
	if installed == 0 {
		return "[]", nil
	}
	state, err := readCurrent(ctx, tx, project)
	if err != nil {
		return "", err
	}
	ids := state.WorkerOrder
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(ids)
	return string(raw), err
}

func validateWorkerOrder(ctx context.Context, tx *sql.Tx, project string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{project}
	for _, id := range ids {
		args = append(args, id)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workers WHERE project_id=? AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...).Scan(&count); err != nil {
		return err
	}
	if count != len(ids) {
		return core.Fail("validation_failed", "Worker order must contain distinct workers from this project.", 400)
	}
	return nil
}

// WorkerOrderCTE accepts ordered IDs and project ID. The tail accepts the last
// worker ID twice and limit+1. IDs remain the scoped cursor anchors; an explicit
// reorder is a live collection change, rather than an immutable list snapshot.
const WorkerOrderCTE = `WITH ordered_workers AS (
 SELECT w.id,w.project_id,w.name,COALESCE(CAST(p.key AS INTEGER),100) AS worker_rank
 FROM workers w LEFT JOIN json_each(?) p ON p.value=w.id WHERE w.project_id=?
) `

const WorkerOrderTail = ` WHERE (?='' OR (w.worker_rank,lower(w.name),w.id) >
 (SELECT worker_rank,lower(name),id FROM ordered_workers WHERE id=?))
 ORDER BY w.worker_rank,lower(w.name),w.id LIMIT ?`

// WorkerOrderCursorKind keeps the selected display ordering distinct from
// other collections while leaving project/principal binding to core cursors.
func WorkerOrderCursorKind(collection string) string { return collection + ":display:v1" }
