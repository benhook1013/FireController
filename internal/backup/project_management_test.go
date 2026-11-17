package backup

import (
	"path/filepath"
	"testing"
)

func TestProjectManagementBackupRetainsOnlyExplicitScopedManagement(t *testing.T) {
	f := newRestoreFixture(t)
	var credential string
	if err := f.store.DB().QueryRow(`SELECT credential_id FROM grants WHERE project_id=? AND can_write=1 LIMIT 1`, f.project).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1,can_read=1,can_write=1 WHERE credential_id=? AND project_id=?`, credential, f.project); err != nil {
		t.Fatal(err)
	}
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
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:project_management'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("management format gate lost", version, err)
	}
	var total, selected, live int
	if err := db.QueryRow(`SELECT COUNT(*) FROM grants WHERE can_manage=1`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM grants WHERE credential_id=? AND project_id=? AND can_manage=1 AND can_read=1 AND can_write=1`, credential, f.project).Scan(&selected); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE revoked=0`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if total != 1 || selected != 1 || live != 0 {
		t.Fatal("management broadened or restored access revived", total, selected, live)
	}
}

func TestProjectManagementBackupRefusesContradictoryGrant(t *testing.T) {
	f := newRestoreFixture(t)
	connection, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connection.ExecContext(t.Context(), `PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.ExecContext(t.Context(), `UPDATE grants SET can_manage=1,can_write=0 WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.ExecContext(t.Context(), `PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles")); err == nil {
		t.Fatal("contradictory management authority accepted")
	}
}
