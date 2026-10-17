package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

const currentRecentRunColumns = `v.id,v.source,v.source_run_id,v.channel,v.head_sha,v.reviewer,v.scope,v.outcome,v.attributable,v.complete,v.started_at,v.finished_at,v.found,v.accepted,v.routed,v.finalized,v.fixed,v.coverage_limits_json`

const currentRecentRunFrom = ` FROM review_current_runs c
 CROSS JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision`

func currentRecentRunsQuery(touched int) string {
	query := `SELECT ` + currentRecentRunColumns + `,c.run_order` + currentRecentRunFrom + ` WHERE c.project_id=? AND c.pull_number=?`
	if touched > 0 {
		query += ` AND c.id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", touched), ",") + `)`
	}
	return query + ` ORDER BY c.run_order DESC LIMIT 3`
}

type orderedCurrentRun struct {
	summary RunSummary
	order   int64
}

func scanOrderedCurrentRun(row interface{ Scan(...any) error }, number int64) (orderedCurrentRun, error) {
	var run orderedCurrentRun
	var limits string
	r := &run.summary
	r.PullNumber = number
	if err := row.Scan(&r.ID, &r.Source, &r.SourceRunID, &r.Channel, &r.HeadSHA, &r.Reviewer, &r.Scope, &r.Outcome, &r.Attributable, &r.Complete, &r.StartedAt, &r.FinishedAt, &r.Found, &r.Accepted, &r.Routed, &r.Finalized, &r.Fixed, &limits, &run.order); err != nil {
		return run, err
	}
	if err := json.Unmarshal([]byte(limits), &r.CoverageLimits); err != nil {
		return run, err
	}
	return run, nil
}

// recentCurrentRuns reduces only the indexed old top three and the bounded
// staged changes. Historical intervals are not scanned, and current references
// remain unchanged until the caller has finished all prospective reads.
func (w *historySnapshotWrite) recentCurrentRuns(ctx context.Context, tx *sql.Tx, number int64) ([]RunSummary, error) {
	changed := []historyCurrentChange{}
	args := []any{w.project, number}
	for _, change := range w.pending {
		if change.spec.current == historyRunRecord.current {
			changed = append(changed, change)
			args = append(args, change.keys[0])
		}
	}
	rows, err := tx.QueryContext(ctx, currentRecentRunsQuery(len(changed)), args...)
	if err != nil {
		return nil, err
	}
	candidates := []orderedCurrentRun{}
	for rows.Next() {
		run, scanErr := scanOrderedCurrentRun(rows, number)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		candidates = append(candidates, run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, change := range changed {
		if change.values == nil || change.values[1] != number {
			continue
		}
		row := tx.QueryRowContext(ctx, `SELECT `+currentRecentRunColumns+`,m.run_order
 FROM review_run_memberships m CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 WHERE m.project_id=? AND m.id=? AND m.start_revision=? AND m.pull_number=?`, w.project, change.keys[0], change.values[0], number)
		run, err := scanOrderedCurrentRun(row, number)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, run)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].order > candidates[j].order })
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	result := make([]RunSummary, 0, len(candidates))
	for _, run := range candidates {
		result = append(result, run.summary)
	}
	return result, nil
}
