package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
	"slices"
)

// Execution owns the full retained configuration. This read-only projection
// selects only ordering policy; it never grants dispatch eligibility.
func queueSortingConfiguration(ctx context.Context, tx *sql.Tx, project string) (string, int64, []int64, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_execution_config')`).Scan(&exists); err != nil {
		return "", 0, nil, err
	}
	if !exists {
		return "auto", 0, nil, nil
	}
	var revision int64
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,body_json FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, project).Scan(&revision, &raw)
	if err == sql.ErrNoRows {
		return "auto", 0, nil, nil
	}
	if err != nil {
		return "", 0, nil, err
	}
	var c struct {
		Mode  string  `json:"queue_sort_mode"`
		Queue []int64 `json:"queue"`
	}
	if len(raw) > 8192 || strictjson.Validate([]byte(raw)) != nil || json.Unmarshal([]byte(raw), &c) != nil || revision < 1 || c.Queue == nil || len(c.Queue) > 100 {
		return "", 0, nil, validationError("Stored queue sorting is unavailable.")
	}
	if c.Mode == "" {
		c.Mode = "auto"
	}
	if c.Mode != "auto" && c.Mode != "manual" {
		return "", 0, nil, validationError("Stored queue sorting is unavailable.")
	}
	seen := map[int64]bool{}
	for _, n := range c.Queue {
		if n < 1 || seen[n] {
			return "", 0, nil, validationError("Stored queue sorting is unavailable.")
		}
		seen[n] = true
	}
	return c.Mode, revision, c.Queue, nil
}
func savedQueueNodes(nodes []queueNode, configured []int64) []queueNode {
	if configured == nil {
		return nodes
	}
	result := slices.Clone(nodes)
	rank := map[int64]int{}
	for i, n := range configured {
		rank[n] = i
	}
	slices.SortStableFunc(result, func(a, b queueNode) int {
		ar, aok := rank[a.number]
		br, bok := rank[b.number]
		if aok && bok {
			return ar - br
		}
		if aok {
			return -1
		}
		if bok {
			return 1
		}
		return 0
	})
	return result
}
