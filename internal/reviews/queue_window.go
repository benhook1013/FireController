package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type queueWindowCursor struct {
	View     string `json:"view"`
	Snapshot string `json:"snapshot"`
	Position int    `json:"position"`
	Cutoff   string `json:"cutoff,omitempty"`
}

func queueWindowAfter(view, after string, now time.Time) (queueWindowCursor, error) {
	value := queueWindowCursor{View: view, Position: -1}
	if view == "recent" {
		value.Cutoff = now.UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	}
	if after == "" {
		return value, nil
	}
	value = queueWindowCursor{}
	if strictjson.Decode([]byte(after), &value) != nil || value.View != view || value.Position < 0 || value.Snapshot == "" || len(value.Snapshot) > 128 {
		return value, validationError("Use a valid cursor from this queue view.")
	}
	if view == "recent" {
		cutoff, err := time.Parse(time.RFC3339Nano, value.Cutoff)
		if err != nil || cutoff.IsZero() || cutoff.UTC().Format(time.RFC3339Nano) != value.Cutoff {
			return value, validationError("Use a valid cursor from this queue view.")
		}
	} else if value.Cutoff != "" {
		return value, validationError("Use a valid cursor from this queue view.")
	}
	canonical, _ := json.Marshal(value)
	if string(canonical) != after {
		return value, validationError("Use a valid cursor from this queue view.")
	}
	return value, nil
}

const compactQueueWindowSQL = `SELECT p.position,p.state,CASE WHEN length(CAST(p.metadata_json AS BLOB))<=512 THEN p.metadata_json ELSE NULL END
	FROM review_pulls p WHERE p.project_id=? AND p.snapshot_id=? AND p.delivery_selected=1 AND NOT EXISTS (
	SELECT 1 FROM review_retired_pulls retired WHERE retired.project_id=p.project_id AND retired.snapshot_id=p.snapshot_id AND retired.number=p.number)`

type queueMergedTime struct {
	position int
	instant  time.Time
}

// Only compact metadata from the selected snapshot is streamed, never run or
// finding archives. Exact Go instants preserve offsets/nanoseconds and validate
// even hidden metadata. The existing Store.Read deadline bounds this O(queue)
// pass; newest retains only two candidates, not the lifetime queue.
func inspectQueueWindow(ctx context.Context, tx *sql.Tx, project, snapshot string) ([2]queueMergedTime, *int, error) {
	newest := [2]queueMergedTime{{position: -1}, {position: -1}}
	rows, err := tx.QueryContext(ctx, compactQueueWindowSQL+` ORDER BY p.position`, project, snapshot)
	if err != nil {
		return newest, nil, err
	}
	defer rows.Close()
	var head *int
	headSeen := false
	for rows.Next() {
		position, state, metadata, err := scanQueueWindow(rows)
		if err != nil {
			return newest, nil, err
		}
		if !headSeen && state != "merged" && state != "closed" {
			headSeen = true
			if state == "open" {
				head = &position
			}
		}
		if metadata.MergedAt == "" {
			continue
		}
		instant, _ := time.Parse(time.RFC3339Nano, metadata.MergedAt)
		candidate := queueMergedTime{position, instant}
		for index := range newest {
			// Ascending configured order is the stable tie breaker.
			if newest[index].position < 0 || instant.After(newest[index].instant) {
				if index == 0 {
					newest[1] = newest[0]
				}
				newest[index] = candidate
				break
			}
		}
	}
	return newest, head, rows.Err()
}

func scanQueueWindow(rows *sql.Rows) (int, string, PullDisplayMetadata, error) {
	var position int
	var state string
	var raw sql.NullString
	if err := rows.Scan(&position, &state, &raw); err != nil {
		return 0, "", PullDisplayMetadata{}, err
	}
	if !raw.Valid {
		return 0, "", PullDisplayMetadata{}, historyInconsistent()
	}
	metadata, err := ReadPullMetadata(raw.String, state)
	return position, state, metadata, err
}

func queueWindowPositions(ctx context.Context, tx *sql.Tx, project string, cursor queueWindowCursor, newest [2]queueMergedTime, limit int) ([]int, error) {
	order, boundary := "ASC", "p.position>?"
	if cursor.View == "merged" {
		order, boundary = "DESC", "(?<0 OR p.position<?)"
	}
	args := []any{project, cursor.Snapshot, cursor.Position}
	if cursor.View == "merged" {
		args = append(args, cursor.Position)
	}
	rows, err := tx.QueryContext(ctx, compactQueueWindowSQL+" AND "+boundary+" ORDER BY p.position "+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cutoff, _ := time.Parse(time.RFC3339Nano, cursor.Cutoff)
	positions := make([]int, 0, limit+1)
	for rows.Next() {
		position, state, metadata, err := scanQueueWindow(rows)
		if err != nil {
			return nil, err
		}
		visible := state == "merged"
		if cursor.View == "recent" {
			instant, _ := time.Parse(time.RFC3339Nano, metadata.MergedAt)
			visible = state != "merged" || metadata.MergedAt == "" || !instant.Before(cutoff) || position == newest[0].position || position == newest[1].position
		}
		if visible {
			positions = append(positions, position)
			if len(positions) == limit+1 {
				break
			}
		}
	}
	return positions, rows.Err()
}

func readQueueWindow(ctx context.Context, store *core.Store, scope core.Scope, view, token string, limit int) (QueueResponse, error) {
	filter := "reviews.queue:" + view + ":v1"
	after, err := store.ParseCursor(scope, filter, token)
	if err != nil {
		return QueueResponse{}, err
	}
	cursor, err := queueWindowAfter(view, after, time.Now())
	if err != nil {
		return QueueResponse{}, err
	}
	response := QueueResponse{ProjectID: scope.ProjectID, Items: make([]PullSummary, 0, limit)}
	err = store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, cursor.Snapshot)
		if err != nil {
			return err
		}
		if !found {
			if cursor.Snapshot != "" {
				return notFound("The review snapshot is unavailable.")
			}
			return nil
		}
		response.Configured = true
		setMetaQueue(&response, meta)
		cursor.Snapshot = meta.SnapshotID
		newest, head, err := inspectQueueWindow(ctx, tx, scope.ProjectID, meta.SnapshotID)
		if err != nil {
			return err
		}
		response.HeadPosition = head
		positions, err := queueWindowPositions(ctx, tx, scope.ProjectID, cursor, newest, limit)
		if err != nil {
			return err
		}
		response.HasMore = len(positions) > limit
		if response.HasMore {
			positions = positions[:limit]
		}
		if len(positions) == 0 {
			return nil
		}
		// Eligibility precedes pagination; only the selected bounded page loads
		// detailed compact summaries. Original positions remain cursor authority.
		placeholders := make([]string, len(positions))
		args := []any{scope.ProjectID, meta.SnapshotID}
		for index, position := range positions {
			placeholders[index] = "?"
			args = append(args, position)
		}
		order := "ASC"
		if view == "merged" {
			order = "DESC"
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT p.number,p.title,p.state,p.head_sha,p.base_ref,p.base_sha,p.ci_state,p.mergeability,p.complete,p.run_count,p.attempt_count,p.recent_runs_json,p.latest_attempts_json,p.metadata_json,p.position FROM review_pulls p WHERE p.project_id=? AND p.snapshot_id=? AND p.delivery_selected=1 AND p.position IN (%s) ORDER BY p.position %s`, strings.Join(placeholders, ","), order), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item PullSummary
			var complete, position int
			var recentJSON, latestJSON, metadataJSON string
			if err := rows.Scan(&item.Number, &item.Title, &item.State, &item.HeadSHA, &item.BaseRef, &item.BaseSHA, &item.CIState, &item.Mergeability, &complete, &item.RunCount, &item.AttemptCount, &recentJSON, &latestJSON, &metadataJSON, &position); err != nil {
				return err
			}
			metadata, err := ReadPullMetadata(metadataJSON, item.State)
			if err != nil {
				return err
			}
			item.Diff, item.IsDraft, item.MergedAt, item.mergeStateStatus = metadata.Diff, metadata.IsDraft, metadata.MergedAt, metadata.MergeStateStatus
			item.QueuePosition, item.Complete = &position, complete != 0
			if err := fillChannelRunCounts(ctx, tx, scope.ProjectID, meta, &item); err != nil {
				return err
			}
			if err := strictStoredJSON(recentJSON, &item.RecentRuns); err != nil {
				return err
			}
			if err := fillRecentRunSources(ctx, tx, scope.ProjectID, meta.SnapshotID, item.Number, item.RecentRuns); err != nil {
				return err
			}
			if err := strictStoredJSON(latestJSON, &item.LatestAttempts); err != nil {
				return err
			}
			response.Items = append(response.Items, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if response.HasMore {
			cursor.Position = positions[len(positions)-1]
			payload, _ := json.Marshal(cursor)
			response.Cursor = store.Cursor(scope, filter, string(payload))
		}
		return nil
	})
	return response, err
}
