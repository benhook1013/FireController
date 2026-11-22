package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func retirementBackupFixture(t *testing.T) (*restoreFixture, string) {
	t.Helper()
	f := newRestoreFixture(t)
	worker := core.NewID("worker")
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, f.project, worker, "Retired worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO worker_retirements(project_id,worker_id,retired_at,actor_id) VALUES(?,?,?,?)`, f.project, worker, "2026-11-01T00:00:00Z", "principal-backup-proof"); err != nil {
		t.Fatal(err)
	}
	return f, worker
}

func TestWorkerRetirementBackupPreservesIdentityAndRetirement(t *testing.T) {
	f, worker := retirementBackupFixture(t)
	bundle := restoreBundle(t, f)
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
			f, _ := retirementBackupFixture(t)
			bundle := restoreBundle(t, f)
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

func TestWorkerNameBackupRestorePreservesRetiredAndActiveIDs(t *testing.T) {
	f, retired := retirementBackupFixture(t)
	active := core.NewID("worker")
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,'Retired worker')`, f.project, active); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if result, err := Restore(t.Context(), bundle.Path, dest); err != nil || result.State != "inactive" {
		t.Fatal(result, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatal("restore changed the base schema version", version, err)
	}
	var activeID, retiredID, retiredAt, actor string
	if err := db.QueryRow(`SELECT w.id FROM workers w WHERE w.project_id=? AND w.name='Retired worker'
		AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)`, f.project).Scan(&activeID); err != nil || activeID != active {
		t.Fatal("active identity changed", activeID, err)
	}
	if err := db.QueryRow(`SELECT w.id,r.retired_at,r.actor_id FROM workers w
		JOIN worker_retirements r ON r.project_id=w.project_id AND r.worker_id=w.id
		WHERE w.project_id=? AND w.name='Retired worker'`, f.project).Scan(&retiredID, &retiredAt, &actor); err != nil || retiredID != retired || retiredAt != "2026-11-01T00:00:00Z" || actor != "principal-backup-proof" {
		t.Fatal("retired identity or attribution changed", retiredID, retiredAt, actor, err)
	}
	var sameName int
	if err := db.QueryRow(`SELECT count(*) FROM workers WHERE project_id=? AND name='Retired worker'`, f.project).Scan(&sameName); err != nil || sameName != 2 {
		t.Fatal("same-name identities not preserved exactly", sameName, err)
	}
}

func TestWorkerNameInventoryRejectsMalformedV6Bundles(t *testing.T) {
	f, _ := retirementBackupFixture(t)
	bundle := restoreBundle(t, f)
	const duplicate = `INSERT INTO workers(project_id,id,name) SELECT project_id,'worker_duplicate',name FROM workers WHERE name='Fixture worker'`
	for _, tc := range []struct{ name, statement string }{
		{"duplicate_active", duplicate},
		{"duplicate_active_without_retirements", `DROP TABLE worker_retirements; DELETE FROM metadata WHERE key='schema:worker_retirements'; ` + duplicate},
		{"wrong_index_order", `DROP INDEX workers_project_name; CREATE INDEX workers_project_name ON workers(name,project_id)`},
		{"unique_index", `DROP INDEX workers_project_name; CREATE UNIQUE INDEX workers_project_name ON workers(project_id,name)`},
		{"changed_table", `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'name TEXT NOT NULL','name TEXT NOT NULL CHECK(length(name)>0)') WHERE type='table' AND name='workers'; PRAGMA writable_schema=OFF`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := restoreModifiedBundle(t, bundle, tc.statement)
			_, err := Inspect(t.Context(), selected)
			backupCode(t, err, "backup_invalid")
			dest := filepath.Join(t.TempDir(), "refused")
			_, err = Restore(t.Context(), selected, dest)
			backupCode(t, err, "backup_invalid")
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatal("invalid bundle left a restore destination", err)
			}
		})
	}
	if _, err := f.store.DB().Exec(duplicate); err != nil {
		t.Fatal(err)
	}
	backupCode(t, ValidateSource(t.Context(), f.dir), "backup_invalid")
}

func TestWorkerNameInventoryAcceptsQuotedV6WithoutRetirements(t *testing.T) {
	f, _ := retirementBackupFixture(t)
	bundle := restoreBundle(t, f)
	selected := restoreModifiedBundle(t, bundle, `DROP TABLE worker_retirements; DELETE FROM metadata WHERE key='schema:worker_retirements';
		PRAGMA writable_schema=ON;
		UPDATE sqlite_schema SET sql=replace(sql,'CREATE TABLE workers(','CREATE TABLE "workers"(') WHERE type='table' AND name='workers';
		PRAGMA writable_schema=OFF`)
	if _, err := Inspect(t.Context(), selected); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), selected, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hasRetirements bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='worker_retirements')
		OR EXISTS(SELECT 1 FROM metadata WHERE key='schema:worker_retirements')`).Scan(&hasRetirements); err != nil || hasRetirements {
		t.Fatal("inactive restore added the optional retirement domain", hasRetirements, err)
	}
}
