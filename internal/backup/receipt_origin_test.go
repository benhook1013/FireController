package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

const receiptsV1Fixture = `CREATE TABLE receipts(principal_id TEXT NOT NULL,project_id TEXT NOT NULL,operation TEXT NOT NULL,target TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,response_json TEXT NOT NULL,PRIMARY KEY(principal_id,project_id,operation,target,key))`
const receiptsV2Fixture = `CREATE TABLE receipts(instance_id TEXT NOT NULL,principal_id TEXT NOT NULL,project_id TEXT NOT NULL,operation TEXT NOT NULL,target TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,response_json TEXT NOT NULL,PRIMARY KEY(instance_id,principal_id,project_id,operation,target,key))`

// Reconstruct the authentic predecessor layout after closing its actual owner.
// This never calls core.Open on a historical snapshot or migrates it to inspect.
func receiptOriginSource(t *testing.T, version int) (*restoreFixture, *sql.DB) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var existing int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != version {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`ALTER TABLE receipts RENAME TO previous_receipts`); err != nil {
			t.Fatal(err)
		}
		statement := receiptsV1Fixture
		if version == 2 {
			statement = receiptsV2Fixture
		}
		if _, err = tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
		columns := "principal_id,project_id,operation,target,key,fingerprint,response_json"
		if version == 2 {
			_, err = tx.Exec(`INSERT INTO receipts(instance_id,`+columns+`) SELECT ?,`+columns+` FROM previous_receipts`, f.store.InstanceID())
		} else {
			_, err = tx.Exec(`INSERT INTO receipts(` + columns + `) SELECT ` + columns + ` FROM previous_receipts`)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`DROP TABLE previous_receipts; PRAGMA user_version=` + fmt.Sprint(version)); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return f, db
}

func TestReceiptOriginBackupVersionsInspectRestoreWithoutRelabeling(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f, db := receiptOriginSource(t, version)
			if version == 2 {
				// Identical old mutation key dimensions may legitimately coexist
				// under two originating installations. Preserve every byte.
				response := " {\n \"literal\":\"<retained> & 雪\", \"revision\":0\n } "
				_, err := db.Exec(`INSERT INTO receipts SELECT ?,principal_id,project_id,operation,target,key,fingerprint,? FROM receipts LIMIT 1`, core.NewID("instance"), response)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := restoreRows(t, db, `SELECT * FROM receipts ORDER BY rowid`)
			if len(before) < 2 {
				t.Fatal("fixture lacks genuine mutation receipts")
			}
			var oldKey string
			if err := db.QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&oldKey); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatalf("real v%d source rejected: %v", version, err)
			}
			bundleBefore := restoreTree(t, bundle.Path)
			sourceBefore := restoreTree(t, f.dir)
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(sourceBefore, restoreTree(t, f.dir)) {
				t.Fatal("inspection changed source or bundle")
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil {
				t.Fatal(err)
			}
			if restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() || restored.SourceInstanceID != f.store.InstanceID() {
				t.Fatal(restored)
			}
			copyDB, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer copyDB.Close()
			var copiedVersion int
			if err = copyDB.QueryRow(`PRAGMA user_version`).Scan(&copiedVersion); err != nil || copiedVersion != version {
				t.Fatalf("restore migrated base schema: %d %v", copiedVersion, err)
			}
			if !reflect.DeepEqual(before, restoreRows(t, copyDB, `SELECT * FROM receipts ORDER BY rowid`)) {
				t.Fatal("restore altered receipt origins, keys, fingerprints or exact response bytes")
			}
			var newKey, predecessor string
			if err = copyDB.QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&newKey); err != nil || oldKey == newKey {
				t.Fatal("signing identity did not rotate", err)
			}
			if err = copyDB.QueryRow(`SELECT value FROM metadata WHERE key='restore:source_instance_id'`).Scan(&predecessor); err != nil || predecessor != f.store.InstanceID() {
				t.Fatal("original v1 migration origin unavailable", err)
			}
			var active int
			if err = copyDB.QueryRow(`SELECT count(*) FROM credentials WHERE revoked!=1`).Scan(&active); err != nil || active != 0 {
				t.Fatal("restored access survived", err)
			}
			if version == 2 {
				var origins, relabeled int
				if err = copyDB.QueryRow(`SELECT count(DISTINCT instance_id) FROM receipts`).Scan(&origins); err != nil || origins != 2 {
					t.Fatal("mixed origins lost", err)
				}
				if err = copyDB.QueryRow(`SELECT count(*) FROM receipts WHERE instance_id=?`, restored.InstanceID).Scan(&relabeled); err != nil || relabeled != 0 {
					t.Fatal("old receipts relabeled to restored identity", err)
				}
			}
			if !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(sourceBefore, restoreTree(t, f.dir)) {
				t.Fatal("restore changed source or original bundle")
			}
		})
	}
}

func TestReceiptOriginInventoryRejectsWrongVersionedSchema(t *testing.T) {
	for _, kind := range []string{"v2_missing_origin", "v1_unexpected_origin", "origin_not_in_pk", "origin_wrong_pk_order", "origin_nullable", "origin_wrong_type", "empty_origin"} {
		t.Run(kind, func(t *testing.T) {
			f, db := receiptOriginSource(t, 1)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "v2_missing_origin" {
				mutateBundleSQL(t, bundle.Path, `PRAGMA user_version=2`)
			} else {
				ddl := receiptsV2Fixture
				switch kind {
				case "origin_not_in_pk":
					ddl = strings.Replace(ddl, "PRIMARY KEY(instance_id,", "PRIMARY KEY(", 1)
				case "origin_wrong_pk_order":
					ddl = strings.Replace(ddl, "PRIMARY KEY(instance_id,principal_id,", "PRIMARY KEY(principal_id,instance_id,", 1)
				case "origin_nullable":
					ddl = strings.Replace(ddl, "instance_id TEXT NOT NULL", "instance_id TEXT", 1)
				case "origin_wrong_type":
					ddl = strings.Replace(ddl, "instance_id TEXT NOT NULL", "instance_id BLOB NOT NULL", 1)
				}
				query := `ALTER TABLE receipts RENAME TO previous_receipts; ` + ddl + `; INSERT INTO receipts SELECT 'retained-origin',principal_id,project_id,operation,target,key,fingerprint,response_json FROM previous_receipts; DROP TABLE previous_receipts; PRAGMA user_version=2`
				if kind == "v1_unexpected_origin" {
					query += `; PRAGMA user_version=1`
				}
				if kind == "empty_origin" {
					query += `; UPDATE receipts SET instance_id=''`
				}
				mutateBundleSQL(t, bundle.Path, query)
			}
			before := restoreTree(t, bundle.Path)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			destination := filepath.Join(t.TempDir(), "invalid-copy")
			_, err = Restore(t.Context(), bundle.Path, destination)
			backupCode(t, err, "backup_invalid")
			if _, err = os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid receipt schema left a destination")
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("rejection modified original bundle")
			}
		})
	}
}
