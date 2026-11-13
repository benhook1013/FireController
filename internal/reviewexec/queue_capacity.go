package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// Completed, unreferenced queue entries can leave scheduling without losing
// Delivery membership or history. Keep the entire prefix of every undispatched
// operation and source refresh, plus the complete queue of a pending selection.
func compactMergedQueue(ctx context.Context, tx *sql.Tx, project string, c Configuration, before, after *int64) ([]int64, error) {
	var projection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_current_pulls')`).Scan(&projection); err != nil || !projection {
		return c.Queue, err
	}
	var selection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND state IN ('queued','selecting'))`, project).Scan(&selection); err != nil || selection {
		return c.Queue, err
	}
	body, err := json.Marshal(c.Queue)
	if err != nil {
		return nil, err
	}
	anchor := int64(0)
	if before != nil {
		anchor = *before
	} else if after != nil {
		anchor = *after
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.value FROM json_each(?) q
	JOIN review_current_pulls p ON p.number=q.value AND p.project_id=?
	JOIN review_pull_memberships m ON m.project_id=p.project_id AND m.number=p.number AND m.start_revision=p.start_revision
	JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision
	JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
	JOIN projects project ON project.id=p.project_id AND project.github_repository_id=s.repository_id
	WHERE v.state='merged' AND q.value<>?
	AND NOT EXISTS (SELECT 1 FROM json_each(?) later JOIN review_operations o ON o.project_id=p.project_id AND o.pull_number=later.value
		WHERE later.key>=q.key AND o.state IN('queued','preparing','posting','active','uncertain'))
	AND NOT EXISTS (SELECT 1 FROM json_each(?) later JOIN review_source_refreshes f ON f.project_id=p.project_id AND f.pull_number=later.value
		WHERE later.key>=q.key AND f.state IN('queued','running'))
	ORDER BY q.key`, string(body), project, anchor, string(body), string(body))
	if err != nil {
		return nil, err
	}
	var merged []int64
	for rows.Next() {
		var number int64
		if err := rows.Scan(&number); err != nil {
			rows.Close()
			return nil, err
		}
		merged = append(merged, number)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, number := range merged {
		pending, err := pendingIncomingRoutes(ctx, tx, project, number)
		if err != nil {
			return nil, err
		}
		carried, err := sourceObligationCount(ctx, tx, project, number)
		if err != nil {
			return nil, err
		}
		if pending == 0 && carried == 0 {
			return slices.DeleteFunc(slices.Clone(c.Queue), func(n int64) bool { return n == number }), nil
		}
	}
	return c.Queue, nil
}
