package tracking

import (
	"context"
	"database/sql"
	"slices"
)

// ValidateSnapshot checks retained tracking records inside a backup or restore
// transaction: every current record decodes canonically, its history is
// contiguous and ends at the current snapshot, and the link index matches.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	if err := validateMilestones(ctx, tx); err != nil {
		return err
	}
	return validateItems(ctx, tx)
}

type recordKey struct{ project, key string }

func keys(ctx context.Context, tx *sql.Tx, table string) ([]recordKey, error) {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,key FROM `+table+` ORDER BY project_id,key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recordKey
	for rows.Next() {
		var k recordKey
		if err = rows.Scan(&k.project, &k.key); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func validateHistory(ctx context.Context, tx *sql.Tx, table, column, project, key string, revision int64, current func(string) (string, error)) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,created_at,`+recordColumn+` FROM `+table+` WHERE project_id=? AND `+column+`=? ORDER BY revision`, project, key)
	if err != nil {
		return err
	}
	defer rows.Close()
	var want int64 = 1
	for rows.Next() {
		var rev int64
		var actor, created string
		var raw sql.NullString
		if err = rows.Scan(&rev, &actor, &created, &raw); err != nil {
			return err
		}
		if rev != want || actor == "" || !raw.Valid {
			return unavailable()
		}
		stamp, err := current(raw.String)
		if err != nil || stamp != created {
			return unavailable()
		}
		want++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if want-1 != revision {
		return unavailable()
	}
	return nil
}

func validateMilestones(ctx context.Context, tx *sql.Tx) error {
	all, err := keys(ctx, tx, "tracking_milestones")
	if err != nil {
		return err
	}
	for _, k := range all {
		m, ok, err := readMilestone(ctx, tx, k.project, k.key)
		if err != nil || !ok {
			return unavailable()
		}
		var last string
		err = validateHistory(ctx, tx, "tracking_milestone_history", "milestone_key", k.project, k.key, m.Revision, func(raw string) (string, error) {
			h, err := decodeMilestone(raw)
			if err != nil || h.Key != k.key || h.ProjectID != k.project || h.CreatedAt != m.CreatedAt {
				return "", unavailable()
			}
			last = raw
			return h.UpdatedAt, nil
		})
		if err != nil {
			return err
		}
		if h, _ := decodeMilestone(last); h != m {
			return unavailable()
		}
	}
	return nil
}

func validateItems(ctx context.Context, tx *sql.Tx) error {
	all, err := keys(ctx, tx, "tracking_items")
	if err != nil {
		return err
	}
	for _, k := range all {
		item, ok, err := readItem(ctx, tx, k.project, k.key)
		if err != nil || !ok {
			return unavailable()
		}
		var last Item
		err = validateHistory(ctx, tx, "tracking_item_history", "item_key", k.project, k.key, item.Revision, func(raw string) (string, error) {
			h, err := decodeItem(raw)
			if err != nil || h.Key != k.key || h.ProjectID != k.project || h.CreatedAt != item.CreatedAt {
				return "", unavailable()
			}
			last = h
			return h.UpdatedAt, nil
		})
		if err != nil || !sameJSON(last, item) {
			return unavailable()
		}
		rows, err := tx.QueryContext(ctx, `SELECT target_type,target_ref,role FROM tracking_item_links WHERE project_id=? AND item_key=? ORDER BY target_type,target_ref,role`, k.project, k.key)
		if err != nil {
			return err
		}
		var indexed []Link
		for rows.Next() {
			var l Link
			if err = rows.Scan(&l.Type, &l.Ref, &l.Role); err != nil {
				rows.Close()
				return err
			}
			indexed = append(indexed, l)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		stored := make([]Link, len(item.Links))
		for i, l := range item.Links {
			stored[i] = Link{Type: l.Type, Ref: l.Ref, Role: l.Role}
		}
		if !slices.Equal(indexed, stored) && !(len(indexed) == 0 && len(stored) == 0) {
			return unavailable()
		}
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tracking_item_links l WHERE NOT EXISTS(SELECT 1 FROM tracking_items i WHERE i.project_id=l.project_id AND i.key=l.item_key)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return unavailable()
	}
	return nil
}
