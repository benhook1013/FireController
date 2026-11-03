package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestWorkerRetirementCoreOnlyStartupAndHistoricalIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,github_repository_id,repository) VALUES('project','Project',1,'owner/repo'); INSERT INTO workers(project_id,id,name) VALUES('project','worker','Worker')`); err != nil {
			return err
		}
		if err := CheckActiveWorker(ctx, tx, "project", "worker"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO worker_retirements(project_id,worker_id,retired_at,actor_id) VALUES('project','worker','2026-11-01T00:00:00Z','synthetic-actor')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal("retirement schema did not survive ordinary startup", err)
	}
	tx, err := store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := CheckWorker(t.Context(), tx, "project", "worker"); err != nil {
		t.Fatal("historical identity was deleted", err)
	}
	receiptCode(t, CheckActiveWorker(t.Context(), tx, "project", "worker"), "worker_retired")
	if err := ValidateWorkerRetirementsSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := tx.QueryRowContext(t.Context(), `SELECT value FROM metadata WHERE key='schema:worker_retirements'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("retirement domain version missing", version, err)
	}
	var domains int
	if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE name IN('jobs','notes','inbox_messages')`).Scan(&domains); err != nil || domains != 0 {
		t.Fatal("active identity check required application domains", domains, err)
	}
}
