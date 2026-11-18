package jobs

import (
	"context"
	"database/sql"
	"net/http"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

// maxBlockers bounds one job's blocked_by list.
const maxBlockers = 20

// readBlockers returns the job ids that block a job, sorted.
func readBlockers(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectID, jobID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT blocker_job_id FROM job_blockers WHERE project_id=? AND job_id=? ORDER BY blocker_job_id`, projectID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// openBlockers counts a job's blockers that are not completed.
func openBlockers(ctx context.Context, tx *sql.Tx, projectID, jobID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM job_blockers b JOIN jobs j ON j.project_id=b.project_id AND j.id=b.blocker_job_id
		WHERE b.project_id=? AND b.job_id=? AND j.status<>'completed'`, projectID, jobID).Scan(&n)
	return n, err
}

// normalizeBlockers validates a requested blocked_by list: bounded, unique,
// existing jobs in the same project, not the job itself and without cycles.
func normalizeBlockers(ctx context.Context, tx *sql.Tx, projectID, jobID string, ids []string) ([]string, error) {
	invalidBlockers := func(message string) error {
		return core.Fail("invalid_blocked_by", message, http.StatusBadRequest)
	}
	if len(ids) > maxBlockers {
		return nil, invalidBlockers("blocked_by accepts at most 20 job ids")
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	if len(slices.Compact(out)) != len(ids) {
		return nil, invalidBlockers("blocked_by contains a duplicate job id")
	}
	for _, id := range out {
		if id == jobID {
			return nil, invalidBlockers("a job cannot block itself")
		}
		if _, err := readJob(ctx, tx, projectID, id); err != nil {
			return nil, invalidBlockers("blocked_by names a job that does not exist in this project")
		}
	}
	// A cycle exists when jobID is reachable from one of its new blockers.
	seen := map[string]bool{}
	stack := slices.Clone(out)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == jobID {
			return nil, invalidBlockers("blocked_by would create a dependency cycle")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		next, err := readBlockers(ctx, tx, projectID, id)
		if err != nil {
			return nil, err
		}
		stack = append(stack, next...)
	}
	return out, nil
}

func replaceBlockers(ctx context.Context, tx *sql.Tx, projectID, jobID string, ids []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_blockers WHERE project_id=? AND job_id=?`, projectID, jobID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_blockers(project_id,job_id,blocker_job_id) VALUES(?,?,?)`, projectID, jobID, id); err != nil {
			return err
		}
	}
	return nil
}

// releaseDependents runs when a job completes. Each dependent job that is
// blocked and now has no open blocker returns to active, in the same
// transaction, so agents see it again. No polling or background worker.
func releaseDependents(ctx context.Context, tx *sql.Tx, scope core.Scope, completedID string, projection func(context.Context, *sql.Tx, string, string) error) error {
	rows, err := tx.QueryContext(ctx, `SELECT j.id FROM job_blockers b JOIN jobs j ON j.project_id=b.project_id AND j.id=b.job_id
		WHERE b.project_id=? AND b.blocker_job_id=? AND j.status='blocked' ORDER BY j.id`, scope.ProjectID, completedID)
	if err != nil {
		return err
	}
	var dependents []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		dependents = append(dependents, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range dependents {
		open, err := openBlockers(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if open > 0 {
			continue
		}
		item, err := readJob(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		item.Status = "active"
		item.Revision++
		item.UpdatedAt = timestamp()
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='active',revision=?,updated_at=? WHERE project_id=? AND id=? AND revision=?`,
			item.Revision, item.UpdatedAt, scope.ProjectID, id, item.Revision-1); err != nil {
			return err
		}
		if err = appendHistory(ctx, tx, scope, item); err != nil {
			return err
		}
		if err = core.AppendEvent(ctx, tx, scope, "job.updated", item.ID, item.Revision,
			map[string]string{"status": item.Status, "worker_id": item.WorkerID, "unblocked_by": completedID}); err != nil {
			return err
		}
		if projection != nil {
			if err = projection(ctx, tx, scope.ProjectID, id); err != nil {
				return err
			}
		}
	}
	return nil
}
