package core

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (s *Store) ensureWorkerRetirements(ctx context.Context) error {
	return s.EnsureSchema(ctx, "worker_retirements", []string{
		`CREATE TABLE worker_retirements(project_id TEXT NOT NULL REFERENCES projects(id),worker_id TEXT NOT NULL,retired_at TEXT NOT NULL,actor_id TEXT NOT NULL,PRIMARY KEY(project_id,worker_id),FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id))`,
	})
}

// CheckActiveWorker is for new coordination targets. CheckWorker and ordinary
// authorization intentionally retain access to historical worker identities.
func CheckActiveWorker(ctx context.Context, tx *sql.Tx, projectID, workerID string) error {
	if err := CheckWorker(ctx, tx, projectID, workerID); err != nil {
		return err
	}
	var retired bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_retirements WHERE project_id=? AND worker_id=?)`, projectID, workerID).Scan(&retired); err != nil {
		return storageError(err)
	}
	if retired {
		return Fail("worker_retired", "This worker is retired; select an active worker for new coordination work.", 409)
	}
	return nil
}

// RequireProjectManagementTx rechecks the actual authenticated management grant
// inside a mutation. Callers also pin the stronger scope before receipt replay.
func (s *Store) RequireProjectManagementTx(ctx context.Context, tx *sql.Tx, scope Scope) error {
	return s.authorize(ctx, tx, scope, "admin")
}

func ValidateWorkerRetirementsSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.retired_at,r.actor_id,w.id FROM worker_retirements r LEFT JOIN workers w ON w.project_id=r.project_id AND w.id=r.worker_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at, actor string
		var worker sql.NullString
		if err := rows.Scan(&at, &actor, &worker); err != nil {
			return err
		}
		timestamp, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || timestamp.IsZero() || at != timestamp.UTC().Format(time.RFC3339Nano) || !worker.Valid || actor == "" || len(actor) > 256 || !utf8.ValidString(actor) || strings.TrimSpace(actor) != actor || strings.IndexFunc(actor, unicode.IsControl) >= 0 {
			return Fail("storage_unavailable", "Stored worker retirement facts are invalid.", 500)
		}
	}
	return rows.Err()
}
