package backup

import (
	"github.com/benhook1013/FireController/internal/core"
	"path/filepath"
	"testing"
)

func retirementBackup(t *testing.T) (*restoreFixture, string, Summary) {
	t.Helper()
	f := newRestoreFixture(t)
	worker := core.NewID("worker")
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, f.project, worker, "Retired worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO worker_retirements(project_id,worker_id,retired_at,actor_id) VALUES(?,?,?,?)`, f.project, worker, "2026-11-01T00:00:00Z", "principal-backup-proof"); err != nil {
		t.Fatal(err)
	}
	return f, worker, restoreBundle(t, f)
}

func TestWorkerRetirementBackupPreservesIdentityAndRetirement(t *testing.T) {
	_, worker, bundle := retirementBackup(t)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, at, actor, version string
	if err := db.QueryRow(`SELECT w.name,r.retired_at,r.actor_id FROM workers w JOIN worker_retirements r ON r.project_id=w.project_id AND r.worker_id=w.id WHERE w.id=?`, worker).Scan(&name, &at, &actor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:worker_retirements'`).Scan(&version); err != nil || version != "1" || name != "Retired worker" || at != "2026-11-01T00:00:00Z" || actor != "principal-backup-proof" {
		t.Fatal(name, at, actor, version, err)
	}
}

func TestWorkerRetirementBackupRejectsMalformedFacts(t *testing.T) {
	for _, statement := range []string{
		`UPDATE worker_retirements SET retired_at='invalid'`,
		`UPDATE worker_retirements SET actor_id=''`,
		`UPDATE metadata SET value='2' WHERE key='schema:worker_retirements'`,
		`DELETE FROM metadata WHERE key='schema:worker_retirements'`,
	} {
		t.Run(statement, func(t *testing.T) {
			_, _, bundle := retirementBackup(t)
			mutateBundleSQL(t, bundle.Path, statement)
			if _, err := Inspect(t.Context(), bundle.Path); err == nil {
				t.Fatal("malformed retirement accepted")
			}
			dest := filepath.Join(t.TempDir(), "must-not-exist")
			if _, err := Restore(t.Context(), bundle.Path, dest); err == nil {
				t.Fatal("malformed retirement restored")
			}
		})
	}
}
