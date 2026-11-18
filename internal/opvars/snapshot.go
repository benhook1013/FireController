package opvars

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/opvarsmodel"
)

// ValidateSnapshot checks every retained operator-variable revision in the
// caller's read transaction: valid keys and values, contiguous revisions from
// one, no repeated consecutive state, a set first revision, known projects and
// the per-scope set-variable bound.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,key,revision,value,actor_id,created_at FROM operator_variable_history ORDER BY project_id,key,revision`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var project, key string
	var last int64
	var previous sql.NullString
	started := false
	set := map[string]int{}
	finish := func() {
		if started && previous.Valid {
			set[project]++
		}
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var p, k, actor, at string
		var rev int64
		var value sql.NullString
		if err = rows.Scan(&p, &k, &rev, &value, &actor, &at); err != nil {
			return err
		}
		if !opvarsmodel.ValidKey(k) {
			return unavailable()
		}
		if _, err = scanEntry(value, rev, actor, at); err != nil {
			return err
		}
		if !started || p != project || k != key {
			finish()
			if rev != 1 || !value.Valid {
				return unavailable()
			}
			project, key, started = p, k, true
		} else if rev != last+1 || value.Valid == previous.Valid && value.String == previous.String {
			return unavailable()
		}
		last, previous = rev, value
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	finish()
	for _, n := range set {
		if n > opvarsmodel.MaxKeys {
			return unavailable()
		}
	}
	var orphans int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM operator_variable_history h LEFT JOIN projects p ON p.id=h.project_id WHERE h.project_id<>'' AND p.id IS NULL`).Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return unavailable()
	}
	return nil
}
