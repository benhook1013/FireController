package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"
)

type queueNode struct {
	number      int64
	position    int
	state, base string
	metadata    PullDisplayMetadata
}

// topologyOrders groups sibling lines by walking each stack depth first.
// Every open row needs a verified branch identity before promoting any row as
// independent. A missing, ambiguous or cyclic parent keeps configured order.
func topologyOrders(nodes []queueNode, repository int64) ([]queueNode, map[int64]int64, bool) {
	type branch struct {
		repository int64
		ref        string
	}
	byBranch := make(map[branch]int64)
	children := make(map[int64][]queueNode)
	parents := make(map[int64]int64)
	roots := []queueNode{}
	delivery := ""
	for _, n := range nodes {
		b := n.metadata.Branches
		if n.state != "open" || n.metadata.IsDraft == nil || b == nil && !*n.metadata.IsDraft {
			return nodes, nil, false
		}
		if b == nil {
			continue
		}
		if delivery != "" && delivery != b.DeliveryRef {
			return nodes, nil, false
		}
		delivery = b.DeliveryRef
		key := branch{b.HeadRepositoryID, b.HeadRef}
		if _, exists := byBranch[key]; exists {
			return nodes, nil, false
		}
		byBranch[key] = n.number
	}
	for _, n := range nodes {
		if n.base == delivery {
			roots = append(roots, n)
			continue
		}
		parent, found := byBranch[branch{repository, n.base}]
		if !found && *n.metadata.IsDraft {
			// A parked draft can target a branch outside the enrolled queue.
			// Keep it behind active stacks without inventing a parent.
			roots = append(roots, n)
			continue
		}
		if !found || parent == n.number {
			return nodes, nil, false
		}
		parents[n.number] = parent
		children[parent] = append(children[parent], n)
	}
	order := make([]queueNode, 0, len(nodes))
	for _, root := range roots {
		if len(children[root.number]) == 0 && !*root.metadata.IsDraft {
			order = append(order, root)
		}
	}
	var drafts []queueNode
	var walk func(queueNode)
	walk = func(n queueNode) {
		if *n.metadata.IsDraft {
			drafts = append(drafts, n)
		} else {
			order = append(order, n)
		}
		for _, child := range children[n.number] {
			walk(child)
		}
	}
	for _, root := range roots {
		if len(children[root.number]) > 0 || *root.metadata.IsDraft {
			walk(root)
		}
	}
	order = append(order, drafts...)
	if len(order) != len(nodes) {
		return nodes, nil, false
	}
	// A draft parent of a ready child must still precede its child.
	seen := map[int64]bool{}
	for _, n := range order {
		if parent := parents[n.number]; parent != 0 && !seen[parent] {
			return nodes, parents, false
		}
		seen[n.number] = true
	}
	return order, parents, true
}

func readQueueNodes(ctx context.Context, tx *sql.Tx, project, snapshot string) ([]queueNode, int64, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT repository_id FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, project, snapshot).Scan(&repository); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.number,p.position,p.state,p.base_ref,p.metadata_json FROM review_pulls p
	WHERE p.project_id=? AND p.snapshot_id=? AND p.delivery_selected=1 AND NOT EXISTS(
	SELECT 1 FROM review_retired_pulls r WHERE r.project_id=p.project_id AND r.snapshot_id=p.snapshot_id AND r.number=p.number) ORDER BY p.position`, project, snapshot)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var nodes []queueNode
	for rows.Next() {
		var n queueNode
		var raw string
		if err := rows.Scan(&n.number, &n.position, &n.state, &n.base, &raw); err != nil {
			return nil, 0, err
		}
		n.metadata, err = ReadPullMetadata(raw, n.state)
		if err != nil {
			return nil, 0, err
		}
		nodes = append(nodes, n)
	}
	return nodes, repository, rows.Err()
}

func orderedRecentNodes(nodes []queueNode, repository int64, cutoff time.Time) ([]queueNode, map[int64]int64) {
	var merged, open, other []queueNode
	unknown := false
	for _, n := range nodes {
		switch n.state {
		case "merged":
			merged = append(merged, n)
		case "open":
			open = append(open, n)
		default:
			other = append(other, n)
			unknown = unknown || n.state == "unknown"
		}
	}
	slices.SortStableFunc(merged, func(a, b queueNode) int {
		at, _ := time.Parse(time.RFC3339Nano, a.metadata.MergedAt)
		bt, _ := time.Parse(time.RFC3339Nano, b.metadata.MergedAt)
		return bt.Compare(at)
	})
	recent := merged[:0]
	for i, n := range merged {
		at, _ := time.Parse(time.RFC3339Nano, n.metadata.MergedAt)
		if i < 2 || at.IsZero() || !at.Before(cutoff) {
			recent = append(recent, n)
		}
	}
	open, parents, complete := topologyOrders(open, repository)
	if !complete || unknown {
		// Preserve relative stored order, including unresolved lifecycle rows.
		open = nil
		for _, n := range nodes {
			if n.state != "merged" {
				open = append(open, n)
			}
		}
		other = nil
	}
	return append(append(recent, open...), other...), parents
}

// OrderedExecutionQueue applies the browser's topology to configured open
// requests. Terminal and unknown entries retain their configured slots.
func OrderedExecutionQueue(ctx context.Context, tx *sql.Tx, project string, configured []int64) ([]int64, error) {
	if len(configured) < 2 {
		return configured, nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_workspace' AND type='table')`).Scan(&exists); err != nil || !exists {
		return configured, err
	}
	selected, err := json.Marshal(configured)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.value,v.state,v.base_ref,v.metadata_json,project.github_repository_id,s.repository_id
	FROM json_each(?) q CROSS JOIN projects project
	LEFT JOIN review_current_pulls p ON p.project_id=project.id AND p.number=q.value
	AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number)
	LEFT JOIN review_pull_memberships m ON m.project_id=p.project_id AND m.number=p.number AND m.start_revision=p.start_revision
	LEFT JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision AND v.delivery_selected=1
	LEFT JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
	WHERE project.id=? ORDER BY q.key`, string(selected), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var open []queueNode
	var slots []int
	var repo int64
	for i := 0; rows.Next(); i++ {
		var n queueNode
		var state, base, raw sql.NullString
		var sourceRepo sql.NullInt64
		if err := rows.Scan(&n.number, &state, &base, &raw, &repo, &sourceRepo); err != nil {
			return nil, err
		}
		if !state.Valid || state.String == "unknown" || !sourceRepo.Valid || sourceRepo.Int64 != repo {
			return configured, nil
		}
		if state.String != "open" {
			continue
		}
		n.state, n.base = state.String, base.String
		n.metadata, err = ReadPullMetadata(raw.String, n.state)
		if err != nil {
			// The normal acquisition path classifies retained display damage.
			// Missing topology alone must not deny unrelated work.
			return configured, nil
		}
		open = append(open, n)
		slots = append(slots, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ordered, _, complete := topologyOrders(open, repo)
	if !complete {
		return configured, nil
	}
	order := slices.Clone(configured)
	for i, slot := range slots {
		order[slot] = ordered[i].number
	}
	return order, nil
}
