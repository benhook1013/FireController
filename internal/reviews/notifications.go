package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

// NotificationSchemaSQL owns current worker preferences, independently of the
// immutable review-history schema and its expensive startup backfill.
func NotificationSchemaSQL() []string {
	return []string{`CREATE TABLE review_pr_followers (
 project_id TEXT NOT NULL REFERENCES projects(id),
 pull_number INTEGER NOT NULL CHECK(pull_number BETWEEN 1 AND 2147483647),
 worker_id TEXT NOT NULL,
 mode TEXT NOT NULL CHECK(mode IN ('auto','follow','muted')),
 revision INTEGER NOT NULL CHECK(revision>=1),
 updated_at TEXT NOT NULL,
 PRIMARY KEY(project_id,pull_number,worker_id),
 FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id)
);`}
}

func EnsureNotificationSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_notifications", NotificationSchemaSQL())
}

type Following struct {
	ProjectID  string `json:"project_id"`
	PullNumber int64  `json:"pull_number"`
	WorkerID   string `json:"worker_id"`
	Mode       string `json:"mode"`
	Revision   int64  `json:"revision"`
	Tracked    bool   `json:"tracked"`
}

func ReadFollowing(ctx context.Context, q HistoryQuery, project, worker string, number int64) (Following, error) {
	v := Following{ProjectID: project, PullNumber: number, WorkerID: worker, Mode: "none"}
	err := q.QueryRowContext(ctx, `SELECT mode,revision FROM review_pr_followers WHERE project_id=? AND pull_number=? AND worker_id=?`, project, number, worker).Scan(&v.Mode, &v.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err == nil && (!oneOf(v.Mode, "auto", "follow", "muted") || v.Revision < 1) {
		err = historyInconsistent()
	}
	return v, err
}

// AutoFollow records actual explicit participation. An existing explicit mute
// wins permanently until that worker deliberately follows again.
func AutoFollow(ctx context.Context, tx *sql.Tx, scope core.Scope, number int64) error {
	if scope.WorkerID == "" {
		return nil
	}
	if number < 1 || number > 2147483647 {
		return validationError("Select a positive 32-bit PR number.")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO review_pr_followers(project_id,pull_number,worker_id,mode,revision,updated_at)
 SELECT ?,?,w.id,'auto',1,? FROM workers w WHERE w.project_id=? AND w.id=?
 AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)
 ON CONFLICT(project_id,pull_number,worker_id) DO NOTHING`, scope.ProjectID, number, time.Now().UTC().Format(time.RFC3339Nano), scope.ProjectID, scope.WorkerID)
	return err
}

// ValidateNotificationPreferences is a read-only backup boundary; exact schema
// definitions and ordinary foreign keys remain the inspection owner's checks.
func ValidateNotificationPreferences(ctx context.Context, q HistoryQuery) error {
	var seeded string
	if err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(value)='text' AND value='1' THEN value END FROM metadata WHERE key='review_notifications:manual_seed_v1'`).Scan(&seeded); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT
 CASE WHEN typeof(f.project_id)='text' AND length(CAST(f.project_id AS BLOB)) BETWEEN 1 AND 128 THEN f.project_id END,
 CASE WHEN typeof(f.pull_number)='integer' AND f.pull_number BETWEEN 1 AND 2147483647 THEN f.pull_number END,
 CASE WHEN typeof(f.worker_id)='text' AND length(CAST(f.worker_id AS BLOB)) BETWEEN 1 AND 128 THEN f.worker_id END,
 CASE WHEN typeof(f.mode)='text' AND f.mode IN ('auto','follow','muted') THEN f.mode END,
 CASE WHEN typeof(f.revision)='integer' AND f.revision>=1 THEN f.revision END,
 CASE WHEN typeof(f.updated_at)='text' AND length(CAST(f.updated_at AS BLOB)) BETWEEN 1 AND 64 THEN f.updated_at END,
 w.id IS NOT NULL
 FROM review_pr_followers f LEFT JOIN workers w ON w.project_id=f.project_id AND w.id=f.worker_id ORDER BY f.project_id,f.pull_number,f.worker_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project, worker, mode, updated string
		var number, revision int64
		var owner bool
		if err := rows.Scan(&project, &number, &worker, &mode, &revision, &updated, &owner); err != nil {
			return err
		}
		at, err := time.Parse(time.RFC3339Nano, updated)
		if project == "" || worker == "" || !owner || number < 1 || number > 2147483647 || !oneOf(mode, "auto", "follow", "muted") || revision < 1 || err != nil || at.IsZero() {
			return historyInconsistent()
		}
	}
	return rows.Err()
}

func publishTerminalNotices(ctx context.Context, tx *sql.Tx, scope core.Scope, snapshot string, number int64, state, title string) error {
	project := scope.ProjectID
	var repository string
	if err := tx.QueryRowContext(ctx, `SELECT repository FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return err
	}
	body := fmt.Sprintf("PR #%d was %s: %s\nhttps://github.com/%s/pull/%d", number, state, title, repository, number)
	key := fmt.Sprintf("pr-terminal:%s:%d:%s", snapshot, number, state)
	return PublishFollowerNotice(ctx, tx, scope, number, key, body)
}

// PublishFollowerNotice delivers trusted transactional PR notices to actual
// participants. Explicit mutes and retirement apply to every notice producer;
// the scope's acting worker never becomes an inferred recipient.
func PublishFollowerNotice(ctx context.Context, tx *sql.Tx, scope core.Scope, number int64, key, body string) error {
	if number < 1 || number > 2147483647 {
		return validationError("Select a positive 32-bit PR number.")
	}
	project := scope.ProjectID
	// Keyset pages keep recipient buffers bounded without truncating delivery.
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT f.worker_id FROM review_pr_followers f WHERE f.project_id=? AND f.pull_number=? AND f.mode IN ('auto','follow') AND f.worker_id>?
 AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=f.project_id AND r.worker_id=f.worker_id) ORDER BY f.worker_id LIMIT 64`, project, number, after)
		if err != nil {
			return err
		}
		workers := []string{}
		for rows.Next() {
			var worker string
			if err = rows.Scan(&worker); err != nil {
				break
			}
			workers = append(workers, worker)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, worker := range workers {
			if _, err := inbox.PublishControllerMessage(ctx, tx, scope, key, worker, body); err != nil {
				return err
			}
			after = worker
		}
		if len(workers) < 64 {
			return nil
		}
	}
}
