package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectManagementBackupRejectsResealedAuthorityCorruption(t *testing.T) {
	for _, statement := range []string{
		`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=sql || replace(hex(zeroblob(8192)),'0',' ') WHERE name='grants'; PRAGMA writable_schema=OFF`,
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
