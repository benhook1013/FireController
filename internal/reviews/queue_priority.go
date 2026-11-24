package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

const QueuePrioritySchema = `CREATE TABLE review_queue_priority (project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),pulls_json TEXT NOT NULL)`

type QueuePriority struct {
	ProjectID     string  `json:"project_id"`
	Revision      int64   `json:"revision"`
	PriorityPulls []int64 `json:"priority_pulls"`
}

func ValidPriorityPulls(pulls []int64) bool {
	if pulls == nil || len(pulls) > 100 {
		return false
	}
	for i, pull := range pulls {
		if pull <= 0 || pull > 2147483647 || i > 0 && pull <= pulls[i-1] {
			return false
		}
	}
	return true
}

func decodeQueuePriority(result *QueuePriority, raw string) error {
	if result.Revision < 1 || len(raw) > 1200 || json.Unmarshal([]byte(raw), &result.PriorityPulls) != nil || !ValidPriorityPulls(result.PriorityPulls) {
		return core.Fail("storage_unavailable", "Stored review queue priority is unavailable.", 503)
	}
	canonical, _ := json.Marshal(result.PriorityPulls)
	if string(canonical) != raw {
		return core.Fail("storage_unavailable", "Stored review queue priority is unavailable.", 503)
	}
	return nil
}

func ReadQueuePriority(ctx context.Context, tx *sql.Tx, project string) (QueuePriority, error) {
	result := QueuePriority{ProjectID: project, PriorityPulls: []int64{}}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_queue_priority')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,CASE WHEN typeof(pulls_json)='text' AND length(CAST(pulls_json AS BLOB))<=1200 THEN pulls_json END FROM review_queue_priority WHERE project_id=?`, project).Scan(&result.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	err = decodeQueuePriority(&result, raw)
	return result, err
}

func ValidateQueuePrioritySnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,revision,CASE WHEN typeof(pulls_json)='text' AND length(CAST(pulls_json AS BLOB))<=1200 THEN pulls_json END FROM review_queue_priority`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var result QueuePriority
		var raw string
		if err := rows.Scan(&result.ProjectID, &result.Revision, &raw); err != nil {
			return err
		}
		if err := decodeQueuePriority(&result, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

func markPriorityNodes(nodes []queueNode, priority QueuePriority) {
	for i := range nodes {
		_, nodes[i].priority = slices.BinarySearch(priority.PriorityPulls, nodes[i].number)
	}
}
