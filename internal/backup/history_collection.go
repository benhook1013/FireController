package backup

import (
	"context"
	"database/sql"
	"strings"

	"github.com/benhook1013/FireController/internal/historycollect"
)

// Collection inspection binds exact owning table and index semantics without
// installing migrations, executing the collector or loading its credentials.
func validateHistoryCollectionPhysicalSchema(ctx context.Context, db sqlReader) error {
	type object struct{ kind, definition string }
	expected := map[string]object{}
	for _, statement := range historycollect.SchemaSQL() {
		fields := strings.Fields(statement)
		if len(fields) < 4 || (fields[1] != "TABLE" && fields[1] != "INDEX") {
			return invalid()
		}
		name := strings.Split(fields[2], "(")[0]
		if _, duplicate := expected[name]; duplicate {
			return invalid()
		}
		expected[name] = object{strings.ToLower(fields[1]), historyDefinitionSQL(statement)}
	}
	if len(expected) != 5 {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE tbl_name IN ('history_collections','history_collection_pages','history_collection_seen') AND (type='table' OR (type='index' AND sql IS NOT NULL)) LIMIT 6`)
	if err != nil {
		if ctx.Err() != nil {
			return unavailable()
		}
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		var definition sql.NullString
		if rows.Scan(&kind, &name, &definition) != nil || !definition.Valid || seen[name] {
			return invalid()
		}
		want, ok := expected[name]
		if !ok || kind != want.kind || historyDefinitionSQL(definition.String) != want.definition {
			return invalid()
		}
		seen[name] = true
	}
	if ctx.Err() != nil {
		return unavailable()
	}
	if rows.Err() != nil || len(seen) != len(expected) {
		return invalid()
	}
	return nil
}
