package core

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerNameMigrationPreservesReferences(t *testing.T) {
	for _, orphaned := range []bool{false, true} {
		label := "preserves history"
		if orphaned {
			label = "rolls back on orphan"
		}
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "firecontroller.sqlite")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			// Build an actual v5 worker table with the old autoindex. The
			// test-only dependent table exercises the SQLite cascade hazard.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range schema {
				if statement == workersV6NameIndex {
					continue
				}
				if statement == workersV6Table {
					statement = strings.TrimSuffix(statement, ")") + ",UNIQUE(project_id,name))"
				}
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			for _, statement := range ConnectorCredentialSchemaSQL() {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO metadata VALUES ('instance_id','instance-test'),('signing_key',?);
				INSERT INTO projects VALUES('project-a','A',1,'owner/a');
				INSERT INTO workers VALUES('project-a','worker-old','Gameplay');
				CREATE TABLE worker_reference(project_id TEXT, worker_id TEXT, FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id) ON DELETE CASCADE);
				INSERT INTO worker_reference VALUES('project-a','worker-old');
				CREATE TRIGGER verify_worker_reference BEFORE INSERT ON worker_reference
					WHEN NOT EXISTS (SELECT 1 FROM workers WHERE id=NEW.worker_id)
					BEGIN SELECT RAISE(ABORT,'missing worker'); END;
				PRAGMA user_version=5`, strings.Repeat("ab", 32)); err != nil {
				t.Fatal(err)
			}
			if orphaned {
				if _, err := db.Exec(`UPDATE worker_reference SET worker_id='missing'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(t.Context(), dir)
			if orphaned {
				if err == nil {
					store.Close()
					t.Fatal("migration accepted a reference with no worker")
				}
				db, err = sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var version int
				var name string
				if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
					t.Fatalf("failed migration changed schema version: %d %v", version, err)
				}
				if err := db.QueryRow(`SELECT name FROM workers WHERE id='worker-old'`).Scan(&name); err != nil || name != "Gameplay" {
					t.Fatalf("failed migration changed original worker: %q %v", name, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var id, name string
			if err := store.DB().QueryRow(`SELECT r.worker_id,w.name FROM worker_reference r JOIN workers w ON (w.project_id,w.id)=(r.project_id,r.worker_id)`).Scan(&id, &name); err != nil || id != "worker-old" || name != "Gameplay" {
				t.Fatalf("retained worker or reference changed: %q %q %v", id, name, err)
			}
			var version, foreignKeys int
			if err := store.writer.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
				t.Fatalf("wrong upgraded schema version: %d %v", version, err)
			}
			if err := store.writer.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("writer foreign keys were not restored: %d %v", foreignKeys, err)
			}
			if _, err := store.writer.Exec(`INSERT INTO worker_reference VALUES('project-a','missing')`); err == nil || !strings.Contains(err.Error(), "missing worker") {
				t.Fatalf("dependent trigger stopped referencing workers: %v", err)
			}
		})
	}
}
