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
	headSHA     string
	baseSHA     string
	state, base string
	metadata    PullDisplayMetadata
}

// topologyOrders sorts only proven components into their existing slots. Draft
// status is dispatch eligibility, not topology. Unresolved components retain
// saved order without preventing independent, fully observed stacks from sorting.
// The boolean reports whole-queue completeness, not whether partial order is useful.
func topologyOrders(nodes []queueNode, repository int64) ([]queueNode, map[int64]int64, bool) {
	type branch struct {
		repository int64
		ref        string
	}
	byBranch := map[branch][]int{}
	observedByBranch := map[branch][]int{}
	parents := map[int64]int64{}
	children := map[int][]int{}
	groups := make([]int, len(nodes))
	invalid := make([]bool, len(nodes))
	parent := make([]int, len(nodes))
	mergedAnchor := make([]bool, len(nodes))
	deliveryRefs := map[string]bool{}
	for i, n := range nodes {
		groups[i], parent[i] = i, -1
		b := n.metadata.Branches
		// Historical delivery targets may be stale. They can qualify an anchor
		// locally, but cannot contradict otherwise current active topology.
		if (n.state == "open" || n.state == "unknown") && b != nil && b.DeliveryRef != "" {
			deliveryRefs[b.DeliveryRef] = true
		}
		invalid[i] = n.state != "open" || b == nil || b.HeadRepositoryID <= 0 || b.HeadRef == "" || b.DeliveryRef == "" || n.base == ""
		if b != nil && b.HeadRepositoryID > 0 && b.HeadRef != "" {
			key := branch{b.HeadRepositoryID, b.HeadRef}
			observedByBranch[key] = append(observedByBranch[key], i)
			if n.state == "open" {
				byBranch[key] = append(byBranch[key], i)
			}
		}
	}
	var find func(int) int
	find = func(i int) int {
		if groups[i] != i {
			groups[i] = find(groups[i])
		}
		return groups[i]
	}
	join := func(a, b int) { groups[find(a)] = find(b) }
	for _, matches := range byBranch {
		if len(matches) > 1 {
			for _, i := range matches {
				invalid[i] = true
				join(matches[0], i)
			}
		}
	}
	for i, n := range nodes {
		if n.state != "open" && n.state != "unknown" {
			continue
		}
		b := n.metadata.Branches
		if b != nil && n.base == b.DeliveryRef {
			continue
		}
		matches := byBranch[branch{repository, n.base}]
		if len(matches) == 0 && b != nil && b.DeliveryRef != "" {
			observed := observedByBranch[branch{repository, n.base}]
			if len(observed) == 1 {
				p := nodes[observed[0]]
				if p.state == "merged" && p.metadata.Branches.DeliveryRef == b.DeliveryRef {
					// A uniquely observed merged parent anchors ordering only. Do not
					// join its pinned history slot, rewrite the base, or invent an
					// active/display dependency on that terminal PR.
					mergedAnchor[i] = true
					continue
				}
			}
		}
		if len(matches) != 1 {
			invalid[i] = true
			for _, j := range matches {
				join(i, j)
			}
			continue
		}
		j := matches[0]
		join(i, j)
		if i == j {
			invalid[i] = true
			continue
		}
		parent[i] = j
		parents[n.number] = nodes[j].number
		children[j] = append(children[j], i)
	}
	bad := map[int]bool{}
	roots := map[int][]int{}
	delivery := map[int]string{}
	for i, n := range nodes {
		g := find(i)
		// Without authoritative project configuration, conflicting observed delivery
		// branches cannot establish which disconnected roots are independent.
		bad[g] = bad[g] || invalid[i] || len(deliveryRefs) > 1
		if parent[i] < 0 {
			roots[g] = append(roots[g], i)
		}
		if b := n.metadata.Branches; b != nil {
			if prior := delivery[g]; prior != "" && prior != b.DeliveryRef {
				bad[g] = true
			}
			delivery[g] = b.DeliveryRef
		}
	}
	// Each node has at most one parent. A connected component with no unique
	// delivery or merged-parent ordering root contains a cycle or unresolved edge,
	// and must remain untouched.
	for i := range nodes {
		g := find(i)
		if len(roots[g]) != 1 {
			bad[g] = true
		}
	}
	var independent, stackRoots []int
	complete := true
	for i := range nodes {
		if bad[find(i)] {
			complete = false
			continue
		}
		if parent[i] < 0 {
			if len(children[i]) == 0 && !mergedAnchor[i] {
				independent = append(independent, i)
			} else {
				stackRoots = append(stackRoots, i)
			}
		}
	}
	ordered := make([]queueNode, 0, len(nodes))
	var walk func(int)
	walk = func(i int) {
		ordered = append(ordered, nodes[i])
		for _, j := range children[i] {
			walk(j)
		}
	}
	for _, i := range independent {
		walk(i)
	}
	for _, i := range stackRoots {
		walk(i)
	}
	result := slices.Clone(nodes)
	next := 0
	for i := range nodes {
		if !bad[find(i)] {
			result[i] = ordered[next]
			next++
		}
	}
	return result, parents, complete
}

func readQueueNodes(ctx context.Context, tx *sql.Tx, project, snapshot string) ([]queueNode, int64, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT repository_id FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, project, snapshot).Scan(&repository); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.number,p.position,p.state,p.head_sha,p.base_ref,p.base_sha,p.metadata_json FROM review_pulls p
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
		if err := rows.Scan(&n.number, &n.position, &n.state, &n.headSHA, &n.base, &n.baseSHA, &raw); err != nil {
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

func mergedQueueNodes(nodes []queueNode) []queueNode {
	merged := make([]queueNode, 0)
	for _, n := range nodes {
		if n.state == "merged" {
			merged = append(merged, n)
		}
	}
	// Equal or missing timestamps keep stored relative order. An unknown
	// time follows known times without dropping the retained row.
	slices.SortStableFunc(merged, func(a, b queueNode) int {
		if a.metadata.MergedAt == "" && b.metadata.MergedAt != "" {
			return 1
		}
		if a.metadata.MergedAt != "" && b.metadata.MergedAt == "" {
			return -1
		}
		at, _ := time.Parse(time.RFC3339Nano, a.metadata.MergedAt)
		bt, _ := time.Parse(time.RFC3339Nano, b.metadata.MergedAt)
		return bt.Compare(at)
	})
	return merged
}

// displayParents resolves each observed branch independently of whole-queue
// ordering readiness. An unrelated missing identity must not erase a known
// parent label. This is display-only and does not authorize execution.
func displayParents(nodes []queueNode, repository int64) map[int64]int64 {
	byBranch := map[string]int64{}
	deliveryRefs := map[string]bool{}
	for _, n := range nodes {
		b := n.metadata.Branches
		if (n.state == "open" || n.state == "unknown") && b != nil {
			deliveryRefs[b.DeliveryRef] = true
		}
		if n.state != "open" || b == nil || b.HeadRepositoryID != repository {
			continue
		}
		if _, exists := byBranch[b.HeadRef]; exists {
			byBranch[b.HeadRef] = 0
		} else {
			byBranch[b.HeadRef] = n.number
		}
	}
	parents := map[int64]int64{}
	for _, n := range nodes {
		if n.state != "open" {
			continue
		}
		if deliveryRefs[n.base] {
			continue
		}
		if parent := byBranch[n.base]; parent != 0 && parent != n.number {
			parents[n.number] = parent
		}
	}
	return parents
}

// parentBaseMovedFromParents marks only comparisons whose same-snapshot open
// parent relationship and exact commit identities are all known. The parent
// map is the existing conservative display relationship, not queue adjacency.
func parentBaseMovedFromParents(nodes []queueNode, repository int64, parents map[int64]int64) map[int64]bool {
	moved := make(map[int64]bool)
	if repository <= 0 {
		return moved
	}
	byNumber := make(map[int64]queueNode, len(nodes))
	for _, n := range nodes {
		byNumber[n.number] = n
	}
	for childNumber, parentNumber := range parents {
		child, childOK := byNumber[childNumber]
		parent, parentOK := byNumber[parentNumber]
		if !childOK || !parentOK || child.state != "open" || parent.state != "open" ||
			child.metadata.Branches == nil || parent.metadata.Branches == nil ||
			child.metadata.Branches.HeadRepositoryID != repository || parent.metadata.Branches.HeadRepositoryID != repository ||
			!canonicalReviewSHA(child.headSHA) || !canonicalReviewSHA(child.baseSHA) ||
			!canonicalReviewSHA(parent.headSHA) || !canonicalReviewSHA(parent.baseSHA) {
			continue
		}
		if child.baseSHA != parent.headSHA {
			moved[childNumber] = true
		}
	}
	return moved
}

func canonicalReviewSHA(value string) bool {
	return value != "" && validSHA(value)
}

func orderedRecentNodes(nodes []queueNode, repository int64, cutoff time.Time) ([]queueNode, map[int64]int64) {
	return orderedRecentNodesMode(nodes, repository, cutoff, "auto")
}
func orderedRecentNodesMode(nodes []queueNode, repository int64, cutoff time.Time, mode string) ([]queueNode, map[int64]int64) {
	if mode == "auto" {
		// Keep terminal metadata available until active ordering is resolved.
		// topologyOrders pins terminal slots; the browser separates history below.
		nodes, _, _ = topologyOrders(nodes, repository)
	}
	var open, other []queueNode
	for _, n := range nodes {
		switch n.state {
		case "merged":
		case "open", "unknown":
			open = append(open, n)
		default:
			other = append(other, n)
		}
	}
	merged := mergedQueueNodes(nodes)
	recent := merged[:0]
	for i, n := range merged {
		at, _ := time.Parse(time.RFC3339Nano, n.metadata.MergedAt)
		if i < 2 || n.metadata.MergedAt == "" || !at.Before(cutoff) {
			recent = append(recent, n)
		}
	}
	parents := displayParents(nodes, repository)
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
	var nodes []queueNode
	var repo int64
	for rows.Next() {
		var n queueNode
		var state, base, raw sql.NullString
		var sourceRepo sql.NullInt64
		if err := rows.Scan(&n.number, &state, &base, &raw, &repo, &sourceRepo); err != nil {
			return nil, err
		}
		if !state.Valid || !sourceRepo.Valid || sourceRepo.Int64 != repo {
			nodes = append(nodes, n) // Untrusted/missing row retains only its configured slot.
			continue
		}
		n.state, n.base = state.String, base.String
		n.metadata, err = ReadPullMetadata(raw.String, n.state)
		if err != nil {
			// The normal acquisition path classifies retained display damage.
			// Missing topology alone must not deny unrelated work.
			n.metadata = PullDisplayMetadata{}
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ordered, _, _ := topologyOrders(nodes, repo)
	order := slices.Clone(configured)
	for i, n := range ordered {
		order[i] = n.number
	}
	return order, nil
}
