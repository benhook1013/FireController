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
	normalized := strings.Join(strings.Fields(definition.String), " ")
	clause := ", " + reviewOrderDDL + ",FOREIGN KEY(project_id,configuration_revision)"
	if version >= 8 {
		clause = ", " + reviewOrderDDL + ", " + reviewFileLimitDDL + ",FOREIGN KEY(project_id,configuration_revision)"
		if strings.Count(normalized, reviewFileLimitDDL) != 1 {
			return invalid()
		}
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
