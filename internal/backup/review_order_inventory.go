package backup

import (
	"context"
	"database/sql"
	"strings"
)

const reviewFileLimitDDL = "override_file_limit INTEGER NOT NULL DEFAULT 0 CHECK(override_file_limit IN (0,1))"

const reviewOrderDDL = "allow_out_of_order INTEGER NOT NULL DEFAULT 0 CHECK(allow_out_of_order IN (0,1))"

func reviewOrderColumn(kind string, notnull, pk int, defaultValue any) bool {
	value, ok := defaultValue.(string)
	return kind == "INTEGER" && notnull == 1 && pk == 0 && ok && value == "0"
}

// Bind the explicit request flags appended in migrations seven and eight,
// without duplicating the predecessor execution schema authority.
func validateReviewOrderPhysicalSchema(ctx context.Context, db sqlReader, version int) error {
	var definition sql.NullString
	if db.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE type='table' AND name='review_operations'`).Scan(&definition) != nil || !definition.Valid {
		return invalid()
	}
	// Supported migration definitions contain no SQL comments. Do not let
	// ignored comment text impersonate the required executable constraints.
	if strings.Contains(definition.String, "/*") || strings.Contains(definition.String, "--") {
		return invalid()
	}
	structural, ok := reviewOrderSQLStructure(definition.String)
	if !ok {
		return invalid()
	}
	normalized := strings.Join(strings.Fields(structural), " ")
	clause := ", " + reviewOrderDDL + ",FOREIGN KEY(project_id,configuration_revision)"
	if version >= 8 {
		clause = ", " + reviewOrderDDL + ", " + reviewFileLimitDDL + ",FOREIGN KEY(project_id,configuration_revision)"
		if strings.Count(normalized, reviewFileLimitDDL) != 1 {
			return invalid()
		}
	}
	var lateIntent bool
	if db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE key='schema:review_intent_rebindings')`).Scan(&lateIntent) != nil {
		return invalid()
	}
	if lateIntent {
		const marker = "intent_predecessor_id TEXT REFERENCES review_operations(id)"
		if strings.Count(normalized, marker) != 1 {
			return invalid()
		}
		clause = strings.Replace(clause, ",FOREIGN KEY", ", "+marker+",FOREIGN KEY", 1)
	}
	if strings.Count(normalized, reviewOrderDDL) != 1 || !strings.Contains(normalized, clause) {
		return invalid()
	}
	return nil
}

func validateReviewOrderRows(ctx context.Context, db sqlReader, version int) error {
	var damaged bool
	if db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE typeof(allow_out_of_order)!='integer' OR allow_out_of_order NOT IN (0,1) LIMIT 1)`).Scan(&damaged) != nil || damaged {
		return invalid()
	}
	if version >= 8 {
		if db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE typeof(override_file_limit)!='integer' OR override_file_limit NOT IN (0,1) LIMIT 1)`).Scan(&damaged) != nil || damaged {
			return invalid()
		}
	}
	return nil
}

// Quoted SQL tokens are data or identifiers, never executable constraint
// syntax. Preserve a separator so stripping cannot join surrounding tokens.
func reviewOrderSQLStructure(definition string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(definition); i++ {
		delimiter := definition[i]
		if delimiter != '\'' && delimiter != '"' && delimiter != '`' && delimiter != '[' {
			out.WriteByte(delimiter)
			continue
		}
		end := delimiter
		if delimiter == '[' {
			end = ']'
		}
		closed := false
		for i++; i < len(definition); i++ {
			if definition[i] != end {
				continue
			}
			if delimiter != '[' && i+1 < len(definition) && definition[i+1] == end {
				i++
				continue
			}
			closed = true
			break
		}
		if !closed {
			return "", false
		}
		out.WriteByte(' ')
	}
	return out.String(), true
}
