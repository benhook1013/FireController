package backup

import (
	"context"
	"database/sql"
	"strings"
)

// These guards preserve the reservation policy through backup and inactive
// restore. Uniqueness must remain account-wide for CLI and repository-scoped
// for Hosted, alongside the exact-target and attempt identities.
func validateReservationScopeSchema(ctx context.Context, db sqlReader) error {
	readError := func() error {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		return invalid()
	}
	type index struct {
		name, origin string
		partial      int
	}
	rows, err := db.QueryContext(ctx, `SELECT name,origin,partial FROM pragma_index_list('review_execution_reservations') WHERE "unique"=1`)
	if err != nil {
		return readError()
	}
	var indexes []index
	for rows.Next() {
		var x index
		if err = rows.Scan(&x.name, &x.origin, &x.partial); err != nil {
			rows.Close()
			return readError()
		}
		indexes = append(indexes, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return readError()
	}
	if len(indexes) != 4 {
		return invalid()
	}
	seen := map[string]bool{}
	for _, x := range indexes {
		var want, identity string
		switch {
		case x.origin == "pk" && x.partial == 0:
			want, identity = "provider repository_id pull_number action_class", "target"
		case x.origin == "u" && x.partial == 0:
			want, identity = "attempt_id", "attempt"
		case x.origin == "c" && x.partial == 1 && x.name == "review_reservations_cli_account":
			want, identity = "provider account action_class", "cli"
		case x.origin == "c" && x.partial == 1 && x.name == "review_reservations_hosted_account_repository":
			want, identity = "provider account repository_id action_class", "hosted"
		default:
			return invalid()
		}
		if seen[identity] {
			return invalid()
		}
		seen[identity] = true
		columns, err := db.QueryContext(ctx, `SELECT name,coll,cid FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno`, x.name)
		if err != nil {
			return readError()
		}
		var names []string
		for columns.Next() {
			var name, coll sql.NullString
			var cid int
			if err = columns.Scan(&name, &coll, &cid); err != nil {
				columns.Close()
				return readError()
			}
			if !name.Valid || !coll.Valid || coll.String != "BINARY" || cid < 0 {
				columns.Close()
				return invalid()
			}
			names = append(names, name.String)
		}
		err = columns.Err()
		columns.Close()
		if err != nil {
			return readError()
		}
		if strings.Join(names, " ") != want {
			return invalid()
		}
		if x.partial == 1 {
			var definition sql.NullString
			if err = db.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(sql AS BLOB))<=4096 THEN sql END FROM sqlite_schema WHERE type='index' AND name=? AND tbl_name='review_execution_reservations'`, x.name).Scan(&definition); err != nil {
				return readError()
			}
			expected := "CREATE UNIQUE INDEX " + x.name + " ON review_execution_reservations(" + strings.ReplaceAll(want, " ", ",") + ") WHERE action_class='" + identity + "'"
			if !definition.Valid || strings.Join(strings.Fields(definition.String), " ") != expected {
				return invalid()
			}
		}
	}
	return nil
}
