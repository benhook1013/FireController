package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const displayStartHeader = "X-FC-Review-Start"

// Read the same first activity shown during execution, bound to the immutable
// capture's attempt. This does not change recorded start/finish or sort order.
func attachDisplayStarts(ctx context.Context, tx *sql.Tx, project string, groups ...[]RunSummary) error {
	runs := make(map[string][]*RunSummary)
	ids := make([]string, 0)
	for _, group := range groups {
		for i := range group {
			run := &group[i]
			if run.Source != nativeSource || (run.Channel != "cli" && run.Channel != "hosted") {
				continue
			}
			if _, seen := runs[run.SourceRunID]; !seen {
				ids = append(ids, run.SourceRunID)
			}
			runs[run.SourceRunID] = append(runs[run.SourceRunID], run)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var available bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_activity_evidence' AND type='table')").Scan(&available); err != nil || !available {
		return err
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,o.pull_number,o.channel,o.head_sha,
		CASE o.channel WHEN 'cli' THEN json_extract(signal.body_json,'$.process_started_at') ELSE json_extract(signal.body_json,'$.observed_at') END
		FROM json_each(?) selected JOIN review_execution_evidence e ON e.id=selected.value AND e.project_id=?
		JOIN review_operations o ON o.id=e.operation_id AND o.project_id=e.project_id
		JOIN review_execution_attempts a ON a.id=e.attempt_id AND a.operation_id=o.id AND a.project_id=e.project_id
		JOIN review_activity_evidence signal ON signal.attempt_id=a.id AND signal.project_id=e.project_id
		WHERE json_extract(signal.body_json,'$.attempt_id')=a.id AND json_extract(signal.body_json,'$.head_sha')=o.head_sha
		AND (o.channel='cli' AND json_extract(signal.body_json,'$.kind')='cli_process_spawned'
		OR o.channel='hosted' AND json_extract(signal.body_json,'$.kind')='hosted_processing_observed' AND json_extract(signal.body_json,'$.trigger_id')=a.external_id)`, string(selected), project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, channel, head string
		var stamp sql.NullString
		var number int64
		if err := rows.Scan(&id, &number, &channel, &head, &stamp); err != nil {
			return err
		}
		signal, err := time.Parse(time.RFC3339Nano, stamp.String)
		if err != nil || signal.IsZero() {
			continue
		}
		for _, run := range runs[id] {
			if run.PullNumber != number || run.HeadSHA != head || run.Channel != channel {
				continue
			}
			start, startErr := time.Parse(time.RFC3339Nano, run.StartedAt)
			finish, finishErr := time.Parse(time.RFC3339Nano, run.FinishedAt)
			if startErr == nil && finishErr == nil && !signal.Before(start) && !signal.After(finish) {
				run.DisplayStartAt = stamp.String
			}
		}
	}
	return rows.Err()
}

func attachQueueDisplayStarts(ctx context.Context, tx *sql.Tx, project string, pulls []PullSummary) error {
	groups := make([][]RunSummary, 0, len(pulls))
	for _, pull := range pulls {
		groups = append(groups, pull.RecentRuns)
	}
	return attachDisplayStarts(ctx, tx, project, groups...)
}
