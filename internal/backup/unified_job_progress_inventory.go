package backup

import (
	"context"
	"database/sql"
	"strings"

	"github.com/benhook1013/FireController/internal/jobs"
)

// Bind schema-eight keys, checks, sequence allocation and indexes to the job
// owner's exact DDL. Older bundles are inspected at their retained version.
func validateUnifiedJobProgressPhysicalSchema(ctx context.Context, db sqlReader) error {
	type object struct{ kind, definition string }
	expected := map[string]object{}
	for _, definition := range jobs.ProgressSchemaSQL() {
		fields := strings.Fields(definition)
		if len(fields) < 4 || fields[0] != "CREATE" {
			return invalid()
		}
		var kind, name string
		switch fields[1] {
		case "TABLE":
			kind, name = "table", strings.SplitN(fields[2], "(", 2)[0]
		case "INDEX":
			kind, name = "index", fields[2]
		default:
			return invalid()
		}
		if _, duplicate := expected[name]; duplicate {
			return invalid()
		}
		expected[name] = object{kind, historyDefinitionSQL(definition)}
	}
	if len(expected) != 3 {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name END,CASE WHEN length(CAST(sql AS BLOB))<=8192 THEN sql END FROM sqlite_schema WHERE tbl_name IN ('job_progress','job_progress_history') AND (type='table' OR (type='index' AND sql IS NOT NULL)) LIMIT 4`)
	if err != nil {
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
		wanted, ok := expected[name]
		if !ok || kind != wanted.kind || historyDefinitionSQL(definition.String) != wanted.definition {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(expected) {
		return invalid()
	}
	return nil
}
