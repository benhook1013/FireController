package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Reconstruct the predecessor's actual table and triggers, not just its marker.
// Both generations retain an active connector and an already revoked connector.
func connectorBackupFixture(t *testing.T, version, bindings int) (*restoreFixture, string) {
	t.Helper()
	f := newRestoreFixture(t)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if version == 4 {
		if _, err = tx.Exec(`DROP TABLE connector_credentials`); err != nil {
			t.Fatal(err)
		}
		for _, definition := range core.ConnectorCredentialV4SchemaSQL() {
			if _, err = tx.Exec(definition); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The fresh store no longer defaults to the v5 connector generation.
	if _, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
		t.Fatal(err)
	}
	active, revoked := core.NewID("credential"), core.NewID("credential")
	for i, id := range []string{active, revoked} {
		if _, err = tx.Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,0,0)`, id, core.NewID("principal"), strings.Repeat(fmt.Sprint(i+2), 64)); err != nil {
			t.Fatal(err)
		}
	}
	// Leave one valid grant/worker without a binding so the resealed fixture can
	// exceed the v5 limit without violating any foreign key or table constraint.
	for i := 0; i <= bindings; i++ {
		project, worker := f.project, f.worker
		if i != 0 {
			project, worker = core.NewID("project"), core.NewID("worker")
			if _, err = tx.Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, project, fmt.Sprintf("Connector project %d", i), 2000+i, fmt.Sprintf("synthetic/connector-%d", i)); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO project_handles(project_id,handle) VALUES(?,?)`, project, fmt.Sprintf("connector-%d", i)); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,'Connector worker')`, project, worker); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = tx.Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,?)`, active, project, i%2); err != nil {
			t.Fatal(err)
		}
		if i < bindings {
			if _, err = tx.Exec(`INSERT INTO connector_credentials(credential_id,project_id,worker_id) VALUES(?,?,?)`, active, project, worker); err != nil {
				t.Fatal(err)
			}
		}
		if i == 0 {
			if _, err = tx.Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,0)`, revoked, project); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO connector_credentials(credential_id,project_id,worker_id) VALUES(?,?,?)`, revoked, project, worker); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = tx.Exec(`UPDATE credentials SET revoked=1 WHERE id=?`, revoked); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f, active
}

func TestConnectorBackupVersionsPreserveBindingsAndRevokeInactiveCopy(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			bindings := 1
			if version == 5 {
				bindings = 16
			}
			f, _ := connectorBackupFixture(t, version, bindings)
			queries := []string{
				`SELECT credential_id,project_id,worker_id FROM connector_credentials ORDER BY credential_id,project_id`,
				`SELECT * FROM grants ORDER BY credential_id,project_id`,
				`SELECT id,principal_id,token_sha256,is_admin FROM credentials ORDER BY id`,
			}
			before := make([][][]any, len(queries))
			for i, query := range queries {
				before[i] = restoreRows(t, f.store.DB(), query)
			}
			if len(before[0]) != bindings+1 {
				t.Fatal("fixture missing connector history")
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSource(t.Context(), f.dir); err != nil {
				t.Fatal("valid versioned source rejected", err)
			}
			bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			sourceBefore, bundleBefore := restoreTree(t, f.dir), restoreTree(t, bundle.Path)
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil {
				t.Fatal(err)
			}
			if restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() || restored.SourceInstanceID != f.store.InstanceID() {
				t.Fatal("restore did not establish a new inactive identity", restored)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var copiedVersion, live int
			if err = db.QueryRow(`PRAGMA user_version`).Scan(&copiedVersion); err != nil || copiedVersion != version {
				t.Fatal("restore migrated the base schema", copiedVersion, err)
			}
			if err = db.QueryRow(`SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&live); err != nil || live != 0 {
				t.Fatal("restore revived copied authority", live, err)
			}
			for i, query := range queries {
				if !reflect.DeepEqual(before[i], restoreRows(t, db, query)) {
					t.Fatalf("restore changed retained connector identities or grants: %s", query)
				}
			}
			if !reflect.DeepEqual(sourceBefore, restoreTree(t, f.dir)) || !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection or restore modified its source")
			}
		})
	}
}

func TestConnectorBackupRejectsResealedSchemaAndBindingCorruption(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			bindings := 1
			definitions, otherDefinitions := core.ConnectorCredentialV4SchemaSQL(), core.ConnectorCredentialSchemaSQL()
			otherVersion := 5
			if version == 5 {
				bindings = 16
				definitions, otherDefinitions = otherDefinitions, definitions
				otherVersion = 4
			}
			f, credential := connectorBackupFixture(t, version, bindings)
			bundle := restoreBundle(t, f)
			cases := []struct{ name, statement string }{
				{"future_base", `PRAGMA user_version=7`},
				{"wrong_base_layout", fmt.Sprintf(`PRAGMA user_version=%d`, otherVersion)},
				{"missing_table", `DROP TABLE connector_credentials`},
				{"extra_index", `CREATE INDEX connector_extra ON connector_credentials(worker_id)`},
				{"admin_binding", `UPDATE credentials SET is_admin=1 WHERE id='` + credential + `'`},
				{"revoked_admin_binding", `UPDATE credentials SET is_admin=1,revoked=1 WHERE id='` + credential + `'`},
				{"missing_credential", `PRAGMA foreign_keys=OFF; DELETE FROM credentials WHERE id='` + credential + `'`},
				{"missing_scoped_grant", `PRAGMA foreign_keys=OFF; DELETE FROM grants WHERE credential_id='` + credential + `'`},
			}
			for _, definition := range definitions[1:] {
				name := strings.Fields(definition)[2]
				cases = append(cases,
					struct{ name, statement string }{"missing_" + name, `DROP TRIGGER ` + name},
					struct{ name, statement string }{"changed_" + name, `DROP TRIGGER ` + name + `; CREATE TRIGGER ` + name + ` BEFORE UPDATE ON connector_credentials BEGIN SELECT 1; END`},
				)
				if name == "connector_binding_update" {
					cases = append(cases, struct{ name, statement string }{"wrong_worker_scope", `PRAGMA foreign_keys=OFF; DROP TRIGGER ` + name + `; UPDATE connector_credentials SET worker_id=(SELECT id FROM workers WHERE project_id<>connector_credentials.project_id LIMIT 1) WHERE credential_id='` + credential + `'; ` + definition})
				}
				if name == "connector_binding_insert" && version == 5 {
					cases = append(cases, struct{ name, statement string }{"seventeen_bindings", `DROP TRIGGER ` + name + `;
						INSERT INTO connector_credentials(credential_id,project_id,worker_id)
						SELECT g.credential_id,g.project_id,w.id FROM grants g JOIN workers w ON w.project_id=g.project_id
						WHERE g.credential_id='` + credential + `' AND NOT EXISTS(SELECT 1 FROM connector_credentials b WHERE b.credential_id=g.credential_id AND b.project_id=g.project_id) LIMIT 1; ` + definition})
				}
			}
			for _, definition := range otherDefinitions[1:] {
				if strings.Fields(definition)[2] == "connector_binding_insert" {
					cases = append(cases, struct{ name, statement string }{"wrong_version_insert_trigger", `DROP TRIGGER connector_binding_insert; ` + definition})
				}
			}
			// Table identity is checked independently of retained binding values.
			tableChanges := []struct{ name, from, to string }{
				{"worker_type", "worker_id TEXT", "worker_id BLOB"},
				{"worker_check", "CHECK(worker_id<>'')", "CHECK(1)"},
				{"grant_scope_fk", "REFERENCES grants(credential_id,project_id)", "REFERENCES grants(credential_id,project_id) ON DELETE CASCADE"},
			}
			if version == 5 {
				tableChanges = append(tableChanges,
					struct{ name, from, to string }{"primary_key_order", "PRIMARY KEY (credential_id,project_id)", "PRIMARY KEY (project_id,credential_id)"},
					struct{ name, from, to string }{"primary_key_worker", "PRIMARY KEY (credential_id,project_id)", "PRIMARY KEY (credential_id,project_id,worker_id)"},
				)
			}
			for _, change := range tableChanges {
				ddl := strings.Replace(definitions[0], change.from, change.to, 1)
				if ddl == definitions[0] {
					t.Fatal("table mutation did not alter canonical DDL", change.name)
				}
				cases = append(cases, struct{ name, statement string }{change.name, `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql='` + strings.ReplaceAll(ddl, "'", "''") + `' WHERE type='table' AND name='connector_credentials'; PRAGMA writable_schema=OFF`})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					selected := restoreModifiedBundle(t, bundle, tc.statement)
					before := restoreTree(t, selected)
					_, err := Inspect(t.Context(), selected)
					backupCode(t, err, "backup_invalid")
					destination := filepath.Join(t.TempDir(), "refused")
					_, err = Restore(t.Context(), selected, destination)
					backupCode(t, err, "backup_invalid")
					if _, err = os.Lstat(destination); !os.IsNotExist(err) {
						t.Fatal("invalid bundle left a restore destination", err)
					}
					if !reflect.DeepEqual(before, restoreTree(t, selected)) {
						t.Fatal("rejection modified the resealed bundle")
					}
				})
			}
		})
	}
}

func TestConnectorBackupSourceRejectsRetainedAdministratorBinding(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f, credential := connectorBackupFixture(t, version, 1)
			if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id=?`, credential); err != nil {
				t.Fatal(err)
			}
			backupCode(t, ValidateSource(t.Context(), f.dir), "backup_invalid")
			_, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
			backupCode(t, err, "backup_invalid")
		})
	}
}
