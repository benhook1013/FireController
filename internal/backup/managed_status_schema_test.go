package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManagedStatusUnconfiguredBeyondBackupInventory(t *testing.T) {
	f := newRestoreFixture(t)
	addManagedDisabledExtension(t, f)
	before := backupRows(t, f.store.DB())
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "unconfigured" || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("status refused unrelated schema or initialized scheduling", status, err)
	}
}

func TestManagedStatusRejectsOwnSchemaWithoutRepair(t *testing.T) {
	canonical := `CREATE TABLE managed_backup_state(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body_json TEXT NOT NULL)`
	for _, tc := range []struct{ name, sql string }{
		{"unsupported-version", `UPDATE metadata SET value='2' WHERE key='schema:managed_backups'`},
		{"noncanonical-version", `UPDATE metadata SET value='01' WHERE key='schema:managed_backups'`},
		{"oversized-version", `UPDATE metadata SET value='` + strings.Repeat("1", 1025) + `' WHERE key='schema:managed_backups'`},
		{"missing-version", `DELETE FROM metadata WHERE key='schema:managed_backups'`},
		{"missing-table", `DROP TABLE managed_backup_state`},
		{"view-instead-of-table", `DROP TABLE managed_backup_state; CREATE VIEW managed_backup_state AS SELECT 1 AS singleton,'{}' AS body_json`},
		{"missing-primary-key", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, " PRIMARY KEY", "", 1)},
		{"missing-singleton-check", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, " CHECK(singleton=1)", "", 1)},
		{"wrong-type", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, "body_json TEXT", "body_json BLOB", 1)},
		{"nullable-body", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, " NOT NULL", "", 1)},
		{"unexpected-default", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, "body_json TEXT NOT NULL", "body_json TEXT NOT NULL DEFAULT '{}'", 1)},
		{"extra-column", `ALTER TABLE managed_backup_state ADD COLUMN extra TEXT`},
		{"generated-column", `ALTER TABLE managed_backup_state ADD COLUMN extra TEXT GENERATED ALWAYS AS (body_json) VIRTUAL`},
		{"unexpected-foreign-key", `DROP TABLE managed_backup_state; ` + strings.Replace(canonical, "body_json TEXT NOT NULL", "body_json TEXT NOT NULL REFERENCES metadata(key)", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			if err := EnsureManagedSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().Exec(tc.sql); err != nil {
				t.Fatal("schema fixture", err)
			}
			before := backupRows(t, f.store.DB())
			schema := restoreRows(t, f.store.DB(), `SELECT type,name,sql FROM sqlite_schema ORDER BY type,name`)
			_, err := ReadStatus(t.Context(), f.dir)
			backupCode(t, err, "backup_invalid")
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(schema, restoreRows(t, f.store.DB(), `SELECT type,name,sql FROM sqlite_schema ORDER BY type,name`)) {
				t.Fatal("status repaired unsupported own schema")
			}
		})
	}
}

func TestManagedStatusPreservesRecoveryAndPathRefusals(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	prior, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err = Restore(t.Context(), prior.LastSuccess.Path, destination); err != nil {
		t.Fatal(err)
	}
	// A fully valid inactive copy remains refused; reporting cannot activate it.
	before := restoreTree(t, destination)
	_, err = ReadStatus(t.Context(), destination)
	backupCode(t, err, "backup_invalid")
	if !reflect.DeepEqual(before, restoreTree(t, destination)) {
		t.Fatal("status changed inactive restore")
	}
	wrong := filepath.Join(t.TempDir(), "absent")
	_, err = ReadStatus(t.Context(), wrong)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(wrong); !os.IsNotExist(err) {
		t.Fatal("status initialized wrong path")
	}
	for _, key := range []string{"instance_id", "signing_key"} {
		var original string
		if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key=?`, key).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE metadata SET value='malformed' WHERE key=?`, key); err != nil {
			t.Fatal(err)
		}
		rows := backupRows(t, f.store.DB())
		_, err = ReadStatus(t.Context(), f.dir)
		backupCode(t, err, "backup_invalid")
		if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) {
			t.Fatal("status repaired invalid installation identity")
		}
		if _, err = f.store.DB().Exec(`UPDATE metadata SET value=? WHERE key=?`, original, key); err != nil {
			t.Fatal(err)
		}
	}
}
