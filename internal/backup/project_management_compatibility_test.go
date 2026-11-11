package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectManagementRealParentBundleRemainsInactive(t *testing.T) {
	root := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	if root == "" {
		t.Skip("requires genuine pinned-parent bundle")
	}
	provenance, err := os.ReadFile(filepath.Join(root, "parent-commit"))
	if err != nil || strings.TrimSpace(string(provenance)) != "383ce8c0bc4505dd89cf5cdec7fb2da1d7e556c8" {
		t.Fatal("fixture must come from pinned predecessor", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "backup_*"))
	if err != nil || len(matches) != 1 {
		t.Fatal("parent bundle missing", err)
	}
	bundle := matches[0]
	if _, err := Inspect(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), bundle, dir); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var live, domains int
	if err := db.QueryRow(`SELECT count(*) FROM credentials WHERE revoked=0`).Scan(&live); err != nil || live != 0 {
		t.Fatal("old credentials revived", live, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM metadata WHERE key IN ('schema:project_management','schema:worker_retirements')`).Scan(&domains); err != nil || domains != 0 {
		t.Fatal("inspection/restore migrated old bundle", domains, err)
	}
}
func TestProjectManagementBackupRejectsResealedAuthorityCorruption(t *testing.T) {
	for _, statement := range []string{
		`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'CHECK(can_manage IN(0,1) AND (can_manage=0 OR (can_read=1 AND can_write=1)))','CHECK(1)') WHERE name='grants'; PRAGMA writable_schema=OFF`,
		`UPDATE metadata SET value='2' WHERE key='schema:project_management'`,
		`DELETE FROM metadata WHERE key='schema:project_management'`,
		`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'DEFAULT 0','DEFAULT 1') WHERE name='grants'; PRAGMA writable_schema=OFF`,
		`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'can_manage INTEGER NOT NULL','can_manage TEXT NOT NULL') WHERE name='grants'; PRAGMA writable_schema=OFF`,
		`PRAGMA ignore_check_constraints=ON; UPDATE grants SET can_manage=0.5; PRAGMA ignore_check_constraints=OFF`,
		`PRAGMA ignore_check_constraints=ON; UPDATE grants SET can_manage=1,can_write=0; PRAGMA ignore_check_constraints=OFF`,
		`PRAGMA foreign_keys=OFF; UPDATE grants SET credential_id='missing'; PRAGMA foreign_keys=ON`,
	} {
		t.Run(statement, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, statement)
			if _, err := Inspect(t.Context(), bundle.Path); err == nil {
				t.Fatal("corrupt management accepted")
			}
			dest := filepath.Join(t.TempDir(), "must-not-exist")
			if _, err := Restore(t.Context(), bundle.Path, dest); err == nil {
				t.Fatal("corrupt management restored")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("target written before validation", err)
			}
		})
	}
}
