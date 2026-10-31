package client

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// InboxSnapshot pairs the ordinary bounded inbox page with the signed
// incoming-message event watermark taken from the same server read snapshot.
type InboxSnapshot struct {
	Inbox       Page[InboxItem] `json:"inbox"`
	EventCursor string          `json:"event_cursor"`
}

// MessageEventPayload contains only the compact metadata supported by the
// incoming feed. Assignment changes have no conversation and may have an acting
// sender worker; message.sent always identifies its sender and conversation.
type MessageEventPayload struct {
	ConversationID    string  `json:"conversation_id,omitempty"`
	RecipientWorkerID string  `json:"recipient_worker_id"`
	SenderWorkerID    *string `json:"sender_worker_id,omitempty"`
}

type MessageEvent struct {
	Sequence   int64               `json:"sequence"`
	ProjectID  string              `json:"project_id"`
	Type       string              `json:"type"`
	ResourceID string              `json:"resource_id"`
	Revision   int64               `json:"revision"`
	ActorID    string              `json:"actor_id"`
	Payload    MessageEventPayload `json:"payload"`
	CreatedAt  string              `json:"created_at"`
}

type MessageEventPage struct {
	Items    []MessageEvent `json:"items"`
	Cursor   string         `json:"cursor"`
	HasMore  bool           `json:"has_more"`
	TimedOut bool           `json:"timed_out"`
}

// InboxSnapshot reads one compact page and its scoped message-feed cursor.
// It does not fetch additional pages or change seen/acknowledged state.
func (c *Client) InboxSnapshot(ctx context.Context, options PageOptions, filter InboxFilter) (InboxSnapshot, error) {
	q, err := pageQuery(options)
	if err != nil {
		return InboxSnapshot{}, err
	}
	if filter.Newest {
		q.Set("newest", "true")
	}
	if filter.Unread {
		q.Set("unread", "true")
	}
	if filter.Unacknowledged {
		q.Set("unacknowledged", "true")
	}
	var value InboxSnapshot
	if err = c.get(ctx, c.projectPath("inbox", c.identity.WorkerID, "snapshot"), q, true, &value); err != nil {
		return InboxSnapshot{}, err
	}
	if !validPage(value.Inbox, options) || !validOpaqueCursor(value.EventCursor) {
		return InboxSnapshot{}, invalidResponse()
	}
	for _, item := range value.Inbox.Items {
		if !c.validInboxItem(item, filter) {
			return InboxSnapshot{}, invalidResponse()
		}
	}
	return value, nil
}

// MessageEvents replays a bounded page after an explicit snapshot/replay
// cursor. A zero limit and waitMS omit those query parameters and use server
// defaults; no cursor is saved or advanced on the caller's behalf.
func (c *Client) MessageEvents(ctx context.Context, cursor string, limit, waitMS int) (MessageEventPage, error) {
	if !validOpaqueCursor(cursor) || limit < 0 || limit > 100 || waitMS < 0 || waitMS > 4000 {
		return MessageEventPage{}, validation()
	}
	q := make(url.Values)
	q.Set("cursor", cursor)
	q.Set("include_assignments", "true")
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if waitMS > 0 {
		q.Set("wait_ms", strconv.Itoa(waitMS))
	}
	var value MessageEventPage
	if err := c.get(ctx, c.projectPath("message-events"), q, true, &value); err != nil {
		return MessageEventPage{}, err
	}
	if value.Items == nil || len(value.Items) > responsePageLimit(limit) || !validOpaqueCursor(value.Cursor) || value.HasMore && len(value.Items) == 0 || value.TimedOut && (len(value.Items) != 0 || value.HasMore) {
		return MessageEventPage{}, invalidResponse()
	}
	var previous int64
	for _, item := range value.Items {
		if !c.validMessageEvent(item) || item.Sequence <= previous {
			return MessageEventPage{}, invalidResponse()
		}
		previous = item.Sequence
	}
	return value, nil
}

func validOpaqueCursor(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func responsePageLimit(limit int) int {
	if limit == 0 {
		return 100
	}
	return limit
}

func (c *Client) validMessageEvent(value MessageEvent) bool {
	if value.Sequence < 1 || value.ProjectID != c.identity.ProjectID || !validID(value.ResourceID) || value.Revision < 1 || !validID(value.ActorID) || !validTimestamp(value.CreatedAt) || value.Payload.RecipientWorkerID != c.identity.WorkerID {
		return false
	}
	switch value.Type {
	case "message.sent":
		return validID(value.Payload.ConversationID) && value.Payload.SenderWorkerID != nil && validID(*value.Payload.SenderWorkerID)
	case "message.seen", "message.acknowledged":
		return validID(value.Payload.ConversationID) && value.Payload.SenderWorkerID == nil
	case "job.assigned", "job.unassigned":
		return value.Payload.ConversationID == "" && (value.Payload.SenderWorkerID == nil || validID(*value.Payload.SenderWorkerID) && *value.Payload.SenderWorkerID != c.identity.WorkerID)
	default:
		return false
	}
}

func validTimestamp(value string) bool {
	if len(value) == 0 || len(value) > 64 || !utf8.ValidString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
