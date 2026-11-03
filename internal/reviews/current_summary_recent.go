package reviews

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// Three admitted summaries, including escaped coverage limits, fit below 512 KiB.
const maxCurrentRecentSummaryBytes = 512 << 10

// Fetch at most each admitted text limit plus one refusal byte. Invalid
// numeric storage must not reach a numeric scanner as an unbounded string.
const currentRecentRunColumns = `CAST(substr(CAST(v.id AS BLOB),1,129) AS TEXT),
 CAST(substr(CAST(v.source AS BLOB),1,129) AS TEXT),
 CAST(substr(CAST(v.source_run_id AS BLOB),1,257) AS TEXT),
 CAST(substr(CAST(v.channel AS BLOB),1,9) AS TEXT),
 CASE WHEN v.head_sha='' THEN '' ELSE CAST(substr(CAST(v.head_sha AS BLOB),1,41) AS TEXT) END,
 CASE WHEN v.reviewer='' THEN '' ELSE CAST(substr(CAST(v.reviewer AS BLOB),1,129) AS TEXT) END,
 CAST(substr(CAST(v.scope AS BLOB),1,129) AS TEXT),
 CAST(substr(CAST(v.outcome AS BLOB),1,13) AS TEXT),
 CASE WHEN typeof(v.attributable)='integer' THEN v.attributable END,
 CASE WHEN typeof(v.complete)='integer' THEN v.complete END,
 CASE WHEN v.started_at='' THEN '' ELSE CAST(substr(CAST(v.started_at AS BLOB),1,65) AS TEXT) END,
 CASE WHEN v.finished_at='' THEN '' ELSE CAST(substr(CAST(v.finished_at AS BLOB),1,65) AS TEXT) END,
 CASE WHEN typeof(v.found)='integer' THEN v.found END,
 CASE WHEN typeof(v.accepted)='integer' THEN v.accepted END,
 CASE WHEN typeof(v.routed)='integer' THEN v.routed END,
 CASE WHEN typeof(v.finalized)='integer' THEN v.finalized END,
 CASE WHEN typeof(v.fixed)='integer' THEN v.fixed END,
 CAST(substr(CAST(v.coverage_limits_json AS BLOB),1,102401) AS TEXT)`

const currentRecentRunFrom = ` FROM review_current_runs c
 CROSS JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision`

func currentRecentRunsQuery(touched int) string {
	query := `SELECT ` + currentRecentRunColumns + `,CASE WHEN typeof(c.run_order)='integer' THEN c.run_order END` + currentRecentRunFrom + ` WHERE c.project_id=? AND c.pull_number=?`
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
	if !validText(r.ID, 128, true) || !validText(r.Source, 128, true) || !validText(r.SourceRunID, 256, true) || !validText(r.Reviewer, 128, false) || !validText(r.Scope, 128, true) || !validSHA(r.HeadSHA) || !validText(r.StartedAt, 64, false) || !validText(r.FinishedAt, 64, false) || !oneOf(r.Channel, "hosted", "cli", "manual", "subagent") || !oneOf(r.Outcome, "completed", "active", "prepared", "posting", "failed", "cancelled", "rate_limited", "uncertain", "unknown") {
		return run, historyInconsistent()
	}
	if len(limits) > 100<<10 || json.Unmarshal([]byte(limits), &r.CoverageLimits) != nil || r.CoverageLimits == nil || len(r.CoverageLimits) > 16 {
		return run, historyInconsistent()
	}
	for _, limit := range r.CoverageLimits {
		if !validText(limit, 1024, true) {
			return run, historyInconsistent()
		}
	}
	return run, nil
}

// Validate only the selected current front. Missing producer source is the
// existing read fallback; every other consumed field and position must match.
// Decode at most three records rather than allocating an attacker-sized array.
func validateCurrentRecentSummary(ctx context.Context, q HistoryQuery, project string, number int64, stored string) error {
	if len(stored) > maxCurrentRecentSummaryBytes {
		return historyInconsistent()
	}
	d := json.NewDecoder(strings.NewReader(stored))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('[') {
		return historyInconsistent()
	}
	previous := make([]RunSummary, 0, 3)
	for d.More() {
		if len(previous) == 3 {
			return historyInconsistent()
		}
		var raw json.RawMessage
		var item RunSummary
		if d.Decode(&raw) != nil || strictjson.Decode(raw, &item) != nil {
			return historyInconsistent()
		}
		previous = append(previous, item)
	}
	closing, err := d.Token()
	if err != nil || closing != json.Delim(']') {
		return historyInconsistent()
	}
	var extra json.RawMessage
	if d.Decode(&extra) != io.EOF {
		return historyInconsistent()
	}
	rows, err := q.QueryContext(ctx, currentRecentRunsQuery(0), project, number)
	if err != nil {
		return err
	}
	expected := make([]RunSummary, 0, 3)
	for rows.Next() {
		run, err := scanOrderedCurrentRun(rows, number)
		if err != nil {
			rows.Close()
			return err
		}
		expected = append(expected, run.summary)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(previous) != len(expected) {
		return historyInconsistent()
	}
	for i := range previous {
		if previous[i].Source == "" {
			previous[i].Source = expected[i].Source
		}
	}
	if mustJSON(previous) != mustJSON(expected) {
		return historyInconsistent()
	}
	return nil
}

// recentCurrentRuns reduces only the indexed old top three and the bounded
// staged changes. Historical intervals are not scanned, and current references
// remain unchanged until the caller has finished all prospective reads.
func (w *historySnapshotWrite) recentCurrentRuns(ctx context.Context, tx HistoryQuery, number int64) ([]RunSummary, error) {
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
		row := tx.QueryRowContext(ctx, `SELECT `+currentRecentRunColumns+`,CASE WHEN typeof(m.run_order)='integer' THEN m.run_order END
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
