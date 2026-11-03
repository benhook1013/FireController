package projectmap

import (
	"context"
	"database/sql"
	"strings"
)

// The v3 CHECK and expression index are required for restored section writes
// and bounded editorial reads. Keep their authority with the migration DDL;
// the backup inventory independently validates columns, keys and foreign keys.
func validateSectionSchema(ctx context.Context, tx *sql.Tx) error {
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil {
		return err
	}
	if version == "1" || version == "2" {
		return nil
	}
	if version != "3" {
		return unavailable()
	}
	normalize := func(sql string) string {
		return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(sql), ";")), " ")
	}
	for _, object := range []struct{ name, kind, sql string }{
		{"project_map_entries", "table", mapEntriesSectionDDL()},
		{"project_map_entries_editorial_order", "index", editorialIndexDDL},
	} {
		var kind, table, definition string
		if err := tx.QueryRowContext(ctx, `SELECT type,tbl_name,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE name=?`, object.name).Scan(&kind, &table, &definition); err != nil {
			return err
		}
		if kind != object.kind || table != "project_map_entries" || normalize(definition) != normalize(object.sql) {
			return unavailable()
		}
	}
	return nil
}
