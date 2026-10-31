package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"
)

const messageFeed = "events:incoming-messages:"

// MessageEventCursor is taken in the caller's already-authorized snapshot.
// It covers all project changes, including gaps filtered from this worker feed.
func (s *Store) MessageEventCursor(ctx context.Context, tx *sql.Tx, scope Scope) (string, error) {
	if scope.WorkerID == "" {
		return "", Fail("validation_failed", "Select a worker for incoming message events.", 400)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?", scope.ProjectID).Scan(&sequence); err != nil {
		return "", storageError(err)
	}
	return s.Cursor(scope, messageFeed+scope.WorkerID, strconv.FormatInt(sequence, 10)), nil
}

type Event struct {
	Sequence   int64           `json:"sequence"`
	ProjectID  string          `json:"project_id"`
	Type       string          `json:"type"`
	ResourceID string          `json:"resource_id"`
	Revision   int64           `json:"revision"`
	ActorID    string          `json:"actor_id"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  string          `json:"created_at"`
}

type EventPage struct {
	Items    []Event `json:"items"`
	Cursor   string  `json:"cursor"`
	HasMore  bool    `json:"has_more"`
	TimedOut bool    `json:"timed_out"`
}

// readMessageEvents rechecks current authorization in the same read transaction
// as both the retained events and the returned watermark.
func (s *Store) readMessageEvents(ctx context.Context, scope Scope, after int64, limit int, includeAssignments bool) (EventPage, error) {
	page := EventPage{Items: make([]Event, 0, limit)}
	err := s.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?", scope.ProjectID).Scan(&head); err != nil {
			return err
		}
		if after > head {
			return Fail("validation_failed", "The event cursor is ahead of this project's history.", 400)
		}
		// after < max(expired sequence) is equivalent to an expired row in
		// the unseen suffix. Seeking that suffix avoids scanning the complete
		// retained project history on every ordinary current-cursor wait.
		var expired bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE project_id=? AND sequence>? AND julianday(created_at)<julianday(?))", scope.ProjectID, after, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return Fail("resync_required", "The event replay window expired; obtain a new scoped snapshot.", 409)
		}
		kinds := "'message.sent','message.seen','message.acknowledged'"
		if includeAssignments {
			kinds += ",'job.assigned','job.unassigned'"
		}
		rows, err := tx.QueryContext(ctx, `SELECT sequence,project_id,event_type,resource_id,revision,actor_id,payload_json,created_at FROM events WHERE project_id=? AND sequence>? AND event_type IN (`+kinds+`) AND json_extract(payload_json,'$.recipient_worker_id')=? ORDER BY sequence LIMIT ?`, scope.ProjectID, after, scope.WorkerID, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var event Event
			var payload string
			if err := rows.Scan(&event.Sequence, &event.ProjectID, &event.Type, &event.ResourceID, &event.Revision, &event.ActorID, &payload, &event.CreatedAt); err != nil {
				return err
			}
			event.Payload = json.RawMessage(payload)
			page.Items = append(page.Items, event)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		page.HasMore = len(page.Items) > limit
		if page.HasMore {
			page.Items = page.Items[:limit]
			head = page.Items[len(page.Items)-1].Sequence
		}
		page.Cursor = s.Cursor(scope, messageFeed+scope.WorkerID, strconv.FormatInt(head, 10))
		return nil
	})
	return page, err
}
