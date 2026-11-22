package core

import (
	"context"
	"database/sql"
	"errors"
)

const workersV6Columns = `(project_id TEXT NOT NULL REFERENCES projects(id),id TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(project_id,id))`

// SQLite quotes the destination table name after an ALTER TABLE RENAME.
// Use the same spelling when creating a new database.
const workersV6Table = `CREATE TABLE "workers"` + workersV6Columns
const workersV6NameIndex = `CREATE INDEX workers_project_name ON workers(project_id,name)`

// WorkerSchemaV6SQL supplies the canonical physical definitions to backup
// inspection. Active-name uniqueness uses the worker retirement domain.
func WorkerSchemaV6SQL() []string {
	return []string{workersV6Table, workersV6NameIndex}
}

func migrateWorkerNames(ctx context.Context, conn *sql.Conn) error {
	for _, statement := range []string{
		"CREATE TABLE workers_v6" + workersV6Columns,
		"INSERT INTO workers_v6(project_id,id,name) SELECT project_id,id,name FROM workers",
		"DROP TABLE workers",
		// Existing triggers may refer to the original workers name, which
		// temporarily does not exist between DROP and RENAME.
		"PRAGMA legacy_alter_table=ON",
		"ALTER TABLE workers_v6 RENAME TO workers",
		"PRAGMA legacy_alter_table=OFF",
		workersV6NameIndex,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return storageError(err)
		}
	}
	var table, parent string
	var rowID sql.NullInt64
	var keyID int
	err := conn.QueryRowContext(ctx, "PRAGMA foreign_key_check").Scan(&table, &rowID, &parent, &keyID)
	if err == nil {
		return Fail("schema_invalid", "Worker name migration would leave an invalid retained reference.", 500)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storageError(err)
	}
	return nil
}
