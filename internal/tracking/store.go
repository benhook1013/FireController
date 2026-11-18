package tracking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, name).Scan(&n)
	return n == 1, err
}

func projectRepository(ctx context.Context, tx *sql.Tx, project string) (string, error) {
	var repository string
	err := tx.QueryRowContext(ctx, `SELECT repository FROM projects WHERE id=?`, project).Scan(&repository)
	if errors.Is(err, sql.ErrNoRows) {
		return "", notFound()
	}
	return repository, err
}

func readItem(ctx context.Context, tx *sql.Tx, project, key string) (Item, bool, error) {
	var status string
	var milestone, parent sql.NullString
	var position, revision int64
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT status,milestone_key,parent_key,position,revision,CASE WHEN length(CAST(record_json AS BLOB))<=? THEN record_json ELSE NULL END FROM tracking_items WHERE project_id=? AND key=?`, maxRecordBytes, project, key).Scan(&status, &milestone, &parent, &position, &revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	if !raw.Valid {
		return Item{}, false, unavailable()
	}
	item, err := decodeItem(raw.String)
	if err != nil {
		return Item{}, false, err
	}
	if item.ProjectID != project || item.Key != key || item.Status != status || item.Milestone != milestone.String || item.Parent != parent.String || item.Position != position || item.Revision != revision || milestone.Valid && milestone.String == "" || parent.Valid && parent.String == "" {
		return Item{}, false, unavailable()
	}
	return item, true, nil
}

func readMilestone(ctx context.Context, tx *sql.Tx, project, key string) (Milestone, bool, error) {
	var state string
	var revision int64
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT state,revision,CASE WHEN length(CAST(record_json AS BLOB))<=? THEN record_json ELSE NULL END FROM tracking_milestones WHERE project_id=? AND key=?`, maxRecordBytes, project, key).Scan(&state, &revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Milestone{}, false, nil
	}
	if err != nil {
		return Milestone{}, false, err
	}
	if !raw.Valid {
		return Milestone{}, false, unavailable()
	}
	m, err := decodeMilestone(raw.String)
	if err != nil {
		return Milestone{}, false, err
	}
	if m.ProjectID != project || m.Key != key || m.State != state || m.Revision != revision {
		return Milestone{}, false, unavailable()
	}
	return m, true, nil
}

// guard applies the optional revision guard: nil skips it, 0 requires
// absence and N requires the current revision N.
func guard(expected *int64, exists bool, current int64) error {
	switch {
	case expected == nil:
		return nil
	case *expected < 0:
		return invalid("expected_revision is 0 (create only) or a current revision.")
	case *expected == 0 && exists:
		return conflict(current)
	case *expected > 0 && !exists:
		return notFound()
	case *expected > 0 && *expected != current:
		return conflict(current)
	}
	return nil
}

func writeItem(ctx context.Context, tx *sql.Tx, scope core.Scope, item Item, create bool) error {
	data, err := json.Marshal(item)
	if err != nil || len(data) > maxRecordBytes {
		return invalid("The item exceeds its 32 KiB stored size.")
	}
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO tracking_items(project_id,key,status,milestone_key,parent_key,position,revision,record_json) VALUES(?,?,?,?,?,?,?,?)`, item.ProjectID, item.Key, item.Status, nullable(item.Milestone), nullable(item.Parent), item.Position, item.Revision, string(data))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE tracking_items SET status=?,milestone_key=?,parent_key=?,position=?,revision=?,record_json=? WHERE project_id=? AND key=?`, item.Status, nullable(item.Milestone), nullable(item.Parent), item.Position, item.Revision, string(data), item.ProjectID, item.Key)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_item_history(project_id,item_key,revision,actor_id,record_json,created_at) VALUES(?,?,?,?,?,?)`, item.ProjectID, item.Key, item.Revision, scope.PrincipalID, string(data), item.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tracking_item_links WHERE project_id=? AND item_key=?`, item.ProjectID, item.Key); err != nil {
		return err
	}
	for _, l := range item.Links {
		if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_item_links(project_id,item_key,target_type,target_ref,role) VALUES(?,?,?,?,?)`, item.ProjectID, item.Key, l.Type, l.Ref, l.Role); err != nil {
			return err
		}
	}
	kind := "tracking.item_updated"
	if create {
		kind = "tracking.item_created"
	}
	return core.AppendEvent(ctx, tx, scope, kind, item.Key, item.Revision, map[string]string{"item_key": item.Key, "status": item.Status})
}

func writeMilestone(ctx context.Context, tx *sql.Tx, scope core.Scope, m Milestone, create bool) error {
	data, err := json.Marshal(m)
	if err != nil || len(data) > maxRecordBytes {
		return invalid("The milestone exceeds its 32 KiB stored size.")
	}
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO tracking_milestones(project_id,key,state,revision,record_json) VALUES(?,?,?,?,?)`, m.ProjectID, m.Key, m.State, m.Revision, string(data))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE tracking_milestones SET state=?,revision=?,record_json=? WHERE project_id=? AND key=?`, m.State, m.Revision, string(data), m.ProjectID, m.Key)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_milestone_history(project_id,milestone_key,revision,actor_id,record_json,created_at) VALUES(?,?,?,?,?,?)`, m.ProjectID, m.Key, m.Revision, scope.PrincipalID, string(data), m.UpdatedAt); err != nil {
		return err
	}
	kind := "tracking.milestone_updated"
	if create {
		kind = "tracking.milestone_created"
	}
	return core.AppendEvent(ctx, tx, scope, kind, m.Key, m.Revision, map[string]string{"milestone_key": m.Key, "state": m.State})
}

// recordExists checks an optional link target table. A table that is not
// installed (for example in a reduced test store) cannot hold the target.
func recordExists(ctx context.Context, tx *sql.Tx, table, project, id string) (bool, error) {
	ok, err := tableExists(ctx, tx, table)
	if err != nil || !ok {
		return false, err
	}
	var n int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE project_id=? AND id=?`, project, id).Scan(&n)
	return n > 0, err
}

// checkTargets validates milestone, parent and every link target against the
// store, normalizing link refs and order in place.
func checkTargets(ctx context.Context, tx *sql.Tx, project, key string, f *ItemFields, repository string, parentChanged bool) error {
	if f.Milestone != "" {
		if _, ok, err := readMilestone(ctx, tx, project, f.Milestone); err != nil {
			return err
		} else if !ok {
			return invalid("Milestone " + f.Milestone + " does not exist in this project; create it first.")
		}
	}
	if f.Parent != "" && parentChanged {
		if err := checkParent(ctx, tx, project, key, f.Parent); err != nil {
			return err
		}
	}
	seen := map[[3]string]bool{}
	links := make([]Link, 0, len(f.Links))
	for _, l := range f.Links {
		n, err := normalizeLinkShape(l, repository)
		if err != nil {
			return err
		}
		id := [3]string{n.Type, n.Ref, n.Role}
		if seen[id] {
			return invalid("Each (type, ref, role) link appears once.")
		}
		seen[id] = true
		if err := checkLinkTarget(ctx, tx, project, key, n); err != nil {
			return err
		}
		links = append(links, n)
	}
	sortLinks(links)
	f.Links = links
	return nil
}

// checkParent refuses cycles and keeps the whole tree within maxParentDepth:
// the new parent's ancestors plus the moved item's own subtree height.
func checkParent(ctx context.Context, tx *sql.Tx, project, key, parent string) error {
	var height int
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE sub(key,depth) AS (SELECT ?,0 UNION ALL SELECT i.key,sub.depth+1 FROM tracking_items i JOIN sub ON i.project_id=? AND i.parent_key=sub.key WHERE sub.depth<=?) SELECT max(depth) FROM sub`, key, project, maxParentDepth).Scan(&height); err != nil {
		return err
	}
	current := parent
	for depth := height; ; depth++ {
		if current == key {
			return invalid("An item cannot be its own ancestor.")
		}
		if depth >= maxParentDepth {
			return invalid("Item nesting is limited to 32 levels.")
		}
		var next sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT parent_key FROM tracking_items WHERE project_id=? AND key=?`, project, current).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			if current == parent {
				return invalid("Parent " + parent + " does not exist in this project; create it first.")
			}
			return unavailable()
		}
		if err != nil {
			return err
		}
		if !next.Valid {
			return nil
		}
		current = next.String
	}
}

func checkLinkTarget(ctx context.Context, tx *sql.Tx, project, key string, l Link) error {
	var table string
	switch l.Type {
	case "job":
		table = "jobs"
	case "note":
		table = "notes"
	case "route":
		table = "review_finding_routes"
	case "item":
		if l.Ref == key {
			return invalid("An item cannot link to itself.")
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tracking_items WHERE project_id=? AND key=?`, project, l.Ref).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return invalid("Linked item " + l.Ref + " does not exist in this project.")
		}
		return nil
	default:
		return nil
	}
	ok, err := recordExists(ctx, tx, table, project, l.Ref)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("Linked " + l.Type + " " + l.Ref + " does not exist in this project.")
	}
	return nil
}

// applyItem writes one item under the optional guard. keepLinks retains the
// current links (or none for a new item) instead of f.Links.
func applyItem(ctx context.Context, tx *sql.Tx, scope core.Scope, repository, key string, expected *int64, f ItemFields, keepLinks bool) (Receipt, error) {
	current, exists, err := readItem(ctx, tx, scope.ProjectID, key)
	if err != nil {
		return Receipt{}, err
	}
	if keepLinks {
		f.Links = []Link{}
		if exists {
			f.Links = slices.Clone(current.Links)
		}
	}
	if f, err = normalizeItem(f); err != nil {
		return Receipt{}, err
	}
	if err = checkTargets(ctx, tx, scope.ProjectID, key, &f, repository, !exists || current.Parent != f.Parent); err != nil {
		return Receipt{}, err
	}
	// A create-only retry of an identical declaration converges instead of
	// conflicting, so a lost response can be retried with a fresh key.
	same := exists && sameJSON(itemFields(current), f)
	if !(same && expected != nil && *expected == 0) {
		if err = guard(expected, exists, current.Revision); err != nil {
			return Receipt{}, err
		}
	}
	if same {
		return Receipt{Kind: "item", Key: key, Revision: current.Revision, Outcome: "unchanged"}, nil
	}
	at := stamp()
	next := Item{Key: key, ProjectID: scope.ProjectID, Revision: 1, Title: f.Title, Description: f.Description, Status: f.Status, Milestone: f.Milestone, Parent: f.Parent, Labels: f.Labels, RevisitWhen: f.RevisitWhen, Position: f.Position, Links: f.Links, CreatedAt: at, UpdatedAt: at}
	if exists {
		next.Revision, next.CreatedAt = current.Revision+1, current.CreatedAt
	}
	if err = writeItem(ctx, tx, scope, next, !exists); err != nil {
		return Receipt{}, err
	}
	outcome := "updated"
	if !exists {
		outcome = "created"
	}
	return Receipt{Kind: "item", Key: key, Revision: next.Revision, Outcome: outcome}, nil
}

func applyMilestone(ctx context.Context, tx *sql.Tx, scope core.Scope, key string, expected *int64, f MilestoneFields) (Receipt, error) {
	current, exists, err := readMilestone(ctx, tx, scope.ProjectID, key)
	if err != nil {
		return Receipt{}, err
	}
	if f, err = normalizeMilestone(f); err != nil {
		return Receipt{}, err
	}
	same := exists && milestoneFields(current) == f
	if !(same && expected != nil && *expected == 0) {
		if err = guard(expected, exists, current.Revision); err != nil {
			return Receipt{}, err
		}
	}
	if same {
		return Receipt{Kind: "milestone", Key: key, Revision: current.Revision, Outcome: "unchanged"}, nil
	}
	at := stamp()
	next := Milestone{Key: key, ProjectID: scope.ProjectID, Revision: 1, Title: f.Title, Description: f.Description, State: f.State, TargetDate: f.TargetDate, Position: f.Position, CreatedAt: at, UpdatedAt: at}
	if exists {
		next.Revision, next.CreatedAt = current.Revision+1, current.CreatedAt
	}
	if err = writeMilestone(ctx, tx, scope, next, !exists); err != nil {
		return Receipt{}, err
	}
	outcome := "updated"
	if !exists {
		outcome = "created"
	}
	return Receipt{Kind: "milestone", Key: key, Revision: next.Revision, Outcome: outcome}, nil
}

func countBy(ctx context.Context, tx *sql.Tx, query string, args ...any) (Counts, error) {
	var c Counts
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err = rows.Scan(&status, &n); err != nil {
			return c, err
		}
		if !validStatus(status) {
			return c, unavailable()
		}
		c.Add(status, n)
	}
	return c, rows.Err()
}
