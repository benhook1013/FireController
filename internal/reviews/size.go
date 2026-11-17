package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/reviewsize"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Size limits are read-only context. The suggestion is a presentation preference;
// the maximum remains owned and enforced by review execution policy.
func readSizeLimits(ctx context.Context, tx *sql.Tx, project string) (reviewsize.Limits, error) {
	var limits reviewsize.Limits
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' AND name IN ('project_presentations','review_policy_config')`)
	if err != nil {
		return limits, err
	}
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return limits, err
		}
		tables[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return limits, err
	}
	for _, spec := range []struct {
		table, query, field string
		value               *int64
	}{
		{"project_presentations", `SELECT state_json FROM project_presentations WHERE project_id=?`, "suggested_max_pr_files", &limits.Suggested},
		{"review_policy_config", `SELECT body_json FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, "max_review_files", &limits.Maximum},
	} {
		if !tables[spec.table] {
			continue
		}
		var raw string
		err = tx.QueryRowContext(ctx, spec.query, project).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return limits, err
		}
		if len(raw) > 20<<10 {
			return limits, historyInconsistent()
		}
		var fields map[string]json.RawMessage
		if strictjson.Decode([]byte(raw), &fields) != nil {
			return limits, historyInconsistent()
		}
		if value, found := fields[spec.field]; found {
			if string(value) == "null" || json.Unmarshal(value, spec.value) != nil || *spec.value < 0 || *spec.value > 100000 {
				return limits, historyInconsistent()
			}
		}
	}
	return limits, nil
}

func storedReviewSize(ctx context.Context, tx *sql.Tx, project, snapshot string, number int64, files *int64, retained int64, limits reviewsize.Limits) *reviewsize.Assessment {
	if limits.Suggested == 0 && limits.Maximum == 0 {
		return nil
	}
	var completed int64
	if retained > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? AND outcome='completed' AND complete=1 AND attributable=1`, project, snapshot, number).Scan(&completed); err != nil {
			return reviewsize.Unavailable(number)
		}
	}
	if completed < 0 || completed > retained {
		return reviewsize.Unavailable(number)
	}
	return reviewsize.Assess(number, limits, files, &completed, retained)
}

func attachReviewSizes(ctx context.Context, tx *sql.Tx, project, snapshot string, items []PullSummary) {
	limits, err := readSizeLimits(ctx, tx, project)
	for i := range items {
		p := &items[i]
		if p.State != "open" && p.State != "unknown" {
			continue
		}
		if err != nil {
			p.ReviewSize = reviewsize.Unavailable(p.Number)
			continue
		}
		var files *int64
		if p.Diff != nil {
			v := p.Diff.Files
			files = &v
		}
		p.ReviewSize = storedReviewSize(ctx, tx, project, snapshot, p.Number, files, p.RunCount, limits)
	}
}

// CurrentReviewSize is bounded local information for enqueue receipts and next
// targets. An unavailable optional read cannot refuse a successful action.
func CurrentReviewSize(ctx context.Context, tx *sql.Tx, project string, number int64) *reviewsize.Assessment {
	return reviewSizeAt(ctx, tx, project, "", number, "")
}

// ReviewSizeForHead never presents another candidate's observed file count as
// sizing for an explicitly requested head. Historical review context remains.
func ReviewSizeForHead(ctx context.Context, tx *sql.Tx, project string, number int64, head string) *reviewsize.Assessment {
	return reviewSizeAt(ctx, tx, project, "", number, head)
}

func reviewSizeAt(ctx context.Context, tx *sql.Tx, project, selectedSnapshot string, number int64, head string) *reviewsize.Assessment {
	limits, err := readSizeLimits(ctx, tx, project)
	if err != nil {
		return reviewsize.Unavailable(number)
	}
	if limits.Suggested == 0 && limits.Maximum == 0 {
		return nil
	}
	var installed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_workspace')`).Scan(&installed); err != nil {
		return reviewsize.Unavailable(number)
	}
	if !installed {
		return reviewsize.Assess(number, limits, nil, nil, 0)
	}
	var snapshot, raw, state, observedHead string
	var retained int64
	if selectedSnapshot == "" {
		err = tx.QueryRowContext(ctx, `SELECT w.snapshot_id,p.metadata_json,p.run_count,p.state,p.head_sha FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=?`, project, number).Scan(&snapshot, &raw, &retained, &state, &observedHead)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT snapshot_id,metadata_json,run_count,state,head_sha FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, project, selectedSnapshot, number).Scan(&snapshot, &raw, &retained, &state, &observedHead)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return reviewsize.Assess(number, limits, nil, nil, 0)
	}
	if err != nil {
		return reviewsize.Unavailable(number)
	}
	metadata, err := ReadPullMetadata(raw, state)
	if err != nil {
		return reviewsize.Unavailable(number)
	}
	var files *int64
	if metadata.Diff != nil && (head == "" || observedHead == head) {
		v := metadata.Diff.Files
		files = &v
	}
	return storedReviewSize(ctx, tx, project, snapshot, number, files, retained, limits)
}
