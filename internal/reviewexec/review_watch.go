package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type ReviewWatchOptions struct {
	PullNumber int64
	Channel    string
	Limit      int
	After      string
}
type ReviewWatchChange struct {
	Sequence  int64            `json:"sequence"`
	EventType string           `json:"event_type"`
	Revision  int64            `json:"revision"`
	Operation OperationSummary `json:"operation"`
}
type ReviewWatchResult struct {
	ProjectID   string              `json:"project_id"`
	Snapshot    *OperationsPage     `json:"snapshot,omitempty"`
	Changes     []ReviewWatchChange `json:"changes"`
	EventCursor string              `json:"event_cursor"`
	HasMore     bool                `json:"has_more"`
	TimedOut    bool                `json:"timed_out"`
}

func reviewWatchFeed(o ReviewWatchOptions) string {
	return "reviews.watch:v1:" + strconv.FormatInt(o.PullNumber, 10) + ":" + o.Channel
}
func validReviewWatch(o ReviewWatchOptions) bool {
	return (o.PullNumber == 0 || validRoutePull(o.PullNumber)) && (o.Channel == "" || validChannel(o.Channel)) && o.Limit >= 1 && o.Limit <= 100 && len(o.After) <= 4096
}
func (e *Engine) reviewWatchChanges(ctx context.Context, scope core.Scope, o ReviewWatchOptions, inner string) (ReviewWatchResult, string, error) {
	out := ReviewWatchResult{ProjectID: scope.ProjectID, Changes: []ReviewWatchChange{}}
	after, err := e.store.ProjectEventPosition(scope, inner)
	if err != nil {
		return out, "", err
	}
	err = e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?`, scope.ProjectID).Scan(&head); err != nil {
			return err
		}
		if after > head {
			return invalid()
		}
		var expired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE project_id=? AND sequence>? AND julianday(created_at)<julianday(?))`, scope.ProjectID, after, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return core.Fail("resync_required", "Review event history expired; obtain a new review watch snapshot and inspect remaining attention pages.", 409)
		}
		query := `SELECT v.sequence,v.event_type,v.revision,o.id,o.pull_number,o.channel,o.head_sha,o.worker_id,o.state,o.reason,o.attempt_id,o.revision,o.created_at,o.updated_at FROM events v LEFT JOIN review_operations o ON o.project_id=v.project_id AND o.id=v.resource_id WHERE v.project_id=? AND v.sequence>? AND v.event_type IN ('review.requested','review.operation_updated')`
		args := []any{scope.ProjectID, after}
		if o.PullNumber != 0 {
			query += ` AND (o.pull_number=? OR o.id IS NULL)`
			args = append(args, o.PullNumber)
		}
		if o.Channel != "" {
			query += ` AND (o.channel=? OR o.id IS NULL)`
			args = append(args, o.Channel)
		}
		query += ` ORDER BY v.sequence LIMIT ?`
		args = append(args, o.Limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ReviewWatchChange
			p := &c.Operation
			if err := rows.Scan(&c.Sequence, &c.EventType, &c.Revision, &p.ID, &p.PullNumber, &p.Channel, &p.HeadSHA, &p.WorkerID, &p.State, &p.Reason, &p.AttemptID, &p.Revision, &p.CreatedAt, &p.UpdatedAt); err != nil {
				return err
			}
			if c.Sequence <= after || c.Revision < 1 || c.Revision > p.Revision || !validOperationSummary(*p) {
				return core.Fail("storage_unavailable", "Stored review changes are unavailable.", 500)
			}
			if len(out.Changes) == o.Limit {
				out.HasMore = true
				head = out.Changes[len(out.Changes)-1].Sequence
				break
			}
			out.Changes = append(out.Changes, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		inner = e.store.ProjectEventCursorAt(scope, head)
		out.EventCursor = e.store.Cursor(scope, reviewWatchFeed(o), inner)
		return nil
	})
	return out, inner, err
}

var errReviewWatchChanged = errors.New("review changes ready")
var errReviewWatchCatchup = errors.New("skip unrelated project backlog")

// WatchReviews owns no provider work. The existing project stream supplies
// commit hints, authorization, subscription capacity and cancellation handling.
func (e *Engine) WatchReviews(ctx context.Context, scope core.Scope, o ReviewWatchOptions, seconds int) (ReviewWatchResult, error) {
	if !validReviewWatch(o) || seconds < 0 || seconds > 900 {
		return ReviewWatchResult{}, invalid()
	}
	if o.After == "" {
		var inner string
		page, err := e.listOperationsSnapshot(ctx, scope, OperationListOptions{PullNumber: o.PullNumber, Channel: o.Channel, Limit: o.Limit, Status: "attention"}, &inner)
		if err != nil {
			return ReviewWatchResult{}, err
		}
		return ReviewWatchResult{ProjectID: scope.ProjectID, Snapshot: &page, Changes: []ReviewWatchChange{}, EventCursor: e.store.Cursor(scope, reviewWatchFeed(o), inner)}, nil
	}
	inner, err := e.store.ParseCursor(scope, reviewWatchFeed(o), o.After)
	if err != nil {
		return ReviewWatchResult{}, err
	}
	waitCtx, cleanup := e.resultWaitContext(ctx)
	defer cleanup()
	deadline, cancel := context.WithTimeout(waitCtx, time.Duration(seconds)*time.Second)
	defer cancel()
	read := func() (ReviewWatchResult, error) {
		if waitCtx.Err() != nil {
			return ReviewWatchResult{}, resultWaitCanceled(waitCtx, "The review activity wait ended; retained work is unchanged.")
		}
		out, next, err := e.reviewWatchChanges(waitCtx, scope, o, inner)
		if waitCtx.Err() != nil {
			return ReviewWatchResult{}, resultWaitCanceled(waitCtx, "The review activity wait ended; retained work is unchanged.")
		}
		if err == nil {
			inner = next
		}
		return out, err
	}
	out, err := read()
	if err != nil || len(out.Changes) > 0 {
		return out, err
	}
	if seconds == 0 {
		out.TimedOut = true
		return out, nil
	}
	for {
		err = e.store.StreamProjectEvents(deadline, scope, inner, 100, func(page core.ProjectEventPage) error {
			var err error
			out, err = read()
			if err != nil {
				return err
			}
			if len(out.Changes) > 0 {
				return errReviewWatchChanged
			}
			// The filtered replay has already advanced across unrelated rows.
			// Rejoin at that watermark instead of draining old metadata pages.
			if page.HasMore {
				return errReviewWatchCatchup
			}
			return nil
		}, func() error { return nil })
		if errors.Is(err, errReviewWatchCatchup) {
			continue
		}
		if errors.Is(err, errReviewWatchChanged) {
			return out, nil
		}
		if waitCtx.Err() != nil {
			return ReviewWatchResult{}, resultWaitCanceled(waitCtx, "The review activity wait ended; retained work is unchanged.")
		}
		if deadline.Err() != nil {
			out, err = read()
			if err == nil {
				out.TimedOut = len(out.Changes) == 0
			}
			return out, err
		}
		return ReviewWatchResult{}, err
	}
}

func (e *Engine) reviewWatchHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	q, err := core.QueryKeys(r, "validation_failed", "pull_number", "channel", "limit", "after")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	o := ReviewWatchOptions{Channel: q.Get("channel"), After: q.Get("after"), Limit: 20}
	if q.Has("pull_number") {
		o.PullNumber, err = strconv.ParseInt(q.Get("pull_number"), 10, 64)
		if err != nil || o.PullNumber < 1 || strconv.FormatInt(o.PullNumber, 10) != q.Get("pull_number") {
			core.WriteError(w, invalid())
			return
		}
	}
	if q.Has("channel") && o.Channel == "" || q.Has("after") && o.After == "" {
		core.WriteError(w, invalid())
		return
	}
	if q.Has("limit") {
		o.Limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || strconv.Itoa(o.Limit) != q.Get("limit") {
			core.WriteError(w, invalid())
			return
		}
	}
	seconds, err := operationPreference(r, 300)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err = operationResponseDeadline(w, seconds); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Add("Vary", "Prefer")
	out, err := e.WatchReviews(r.Context(), scope, o, seconds)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
