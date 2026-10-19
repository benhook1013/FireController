package core

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

const projectFeed = "events:project-metadata"

// These committed state changes may invalidate private project views. Keep the
// exact allowlist shared by replay and live hints; messages and authentication
// events retain their separate delivery/authorization paths.
var projectEventTypes = [...]string{
	"coordination.imported",
	"project.presentation_changed", "map.entry_created", "map.entry_updated",
	"job.workstream_changed", "job.created", "job.updated", "job.checkpointed", "job.update_appended", "job.checklist_replaced",
	"workers.created", "worker.pause_changed", "worker.primary_job_changed",
	"note.created", "note.updated",
	"review.observed", "reviews.policy_configured", "reviews.rounds_configured", "review.requested", "review.operation_updated",
	"review.evidence.adjudicated", "review.evidence.finalized", "review.evidence.fix_recorded", "review.evidence.routed",
	"review.route.routed", "review.route.superseded", "review.route.adjudicated", "review.route.fix_recorded",
}

var projectEventsQuery = `SELECT sequence,event_type,resource_id,revision FROM events WHERE project_id=? AND sequence>? AND event_type IN (` + strings.TrimSuffix(strings.Repeat("?,", len(projectEventTypes)), ",") + `) ORDER BY sequence LIMIT ?`

func projectEventType(kind string) bool {
	for _, allowed := range projectEventTypes {
		if kind == allowed {
			return true
		}
	}
	return false
}

// ProjectEventCursor belongs to the caller's already-authorized snapshot. Its
// namespace is project-wide and does not depend on the selected worker.
func (s *Store) ProjectEventCursor(ctx context.Context, tx *sql.Tx, scope Scope) (string, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?", scope.ProjectID).Scan(&sequence); err != nil {
		return "", storageError(err)
	}
	return s.Cursor(scope, projectFeed, strconv.FormatInt(sequence, 10)), nil
}

type ProjectEventItem struct {
	EventType   string `json:"event_type"`
	ResourceID  string `json:"resource_id"`
	Revision    int64  `json:"revision"`
	EventCursor string `json:"event_cursor"`
}

type ProjectEventPage struct {
	Items       []ProjectEventItem `json:"items"`
	EventCursor string             `json:"event_cursor"`
	HasMore     bool               `json:"has_more"`
}

func (s *Store) readProjectEvents(ctx context.Context, scope Scope, after int64, limit int) (ProjectEventPage, error) {
	page := ProjectEventPage{Items: make([]ProjectEventItem, 0, limit)}
	err := s.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?", scope.ProjectID).Scan(&head); err != nil {
			return err
		}
		if after > head {
			return Fail("validation_failed", "The event cursor is ahead of this project's history.", 400)
		}
		var expired bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE project_id=? AND sequence>? AND julianday(created_at)<julianday(?))", scope.ProjectID, after, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return Fail("resync_required", "The event replay window expired; obtain a new scoped snapshot.", 409)
		}
		args := make([]any, 0, len(projectEventTypes)+3)
		args = append(args, scope.ProjectID, after)
		for _, kind := range projectEventTypes {
			args = append(args, kind)
		}
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, projectEventsQuery, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var last int64
		for rows.Next() {
			var sequence int64
			var item ProjectEventItem
			if err := rows.Scan(&sequence, &item.EventType, &item.ResourceID, &item.Revision); err != nil {
				return err
			}
			item.EventCursor = s.Cursor(scope, projectFeed, strconv.FormatInt(sequence, 10))
			page.Items = append(page.Items, item)
			if len(page.Items) <= limit {
				last = sequence
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		page.HasMore = len(page.Items) > limit
		if page.HasMore {
			page.Items = page.Items[:limit]
			head = last
		}
		page.EventCursor = s.Cursor(scope, projectFeed, strconv.FormatInt(head, 10))
		return nil
	})
	return page, err
}

// StreamProjectEvents delivers only compact private project changes. It shares stream
// ownership and transport-independent lifecycle with incoming messages.
func (s *Store) StreamProjectEvents(ctx context.Context, scope Scope, cursor string, limit int, deliver func(ProjectEventPage) error, heartbeat func() error) error {
	return s.streamProjectEvents(ctx, scope, cursor, limit, deliver, heartbeat, 15*time.Second)
}

func (s *Store) streamProjectEvents(ctx context.Context, scope Scope, cursor string, limit int, deliver func(ProjectEventPage) error, heartbeat func() error, interval time.Duration) error {
	if cursor == "" || limit < 1 || limit > 100 || deliver == nil || heartbeat == nil {
		return Fail("validation_failed", "Use a snapshot cursor, limit 1-100 and stream callbacks.", 400)
	}
	return s.streamEvents(ctx, scope, cursor, projectFeed, messageTopic{project: scope.ProjectID, feed: projectFeed}, func(after int64) (streamEventPage, error) {
		page, err := s.readProjectEvents(ctx, scope, after, limit)
		return streamEventPage{page.EventCursor, page.HasMore, len(page.Items), func() error { return deliver(page) }}, err
	}, heartbeat, interval)
}
