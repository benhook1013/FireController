package client

import (
	"context"
	"unicode/utf8"
)

// MessageDelivery is the selected message's current recipient receipt, without
// its body or another worker's inbox. Reading never changes receipt state.
type MessageDelivery struct {
	ID                string `json:"id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Seen              bool   `json:"seen"`
	Acknowledged      bool   `json:"acknowledged"`
}

// Outbox discovers messages sent by the bound worker. Filters concern their
// actual recipients' receipt state; incoming inbox selection remains separate.
func (c *Client) Outbox(ctx context.Context, options PageOptions, filter InboxFilter, selectedWorker ...string) (Page[InboxItem], error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return Page[InboxItem]{}, err
	}
	q, err := pageQuery(options)
	if err != nil {
		return Page[InboxItem]{}, err
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
	var value Page[InboxItem]
	if err = c.get(ctx, c.projectPath("outbox", worker), q, true, &value); err != nil {
		return Page[InboxItem]{}, err
	}
	if !validPage(value, options) {
		return Page[InboxItem]{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !validID(item.ID) || !validID(item.ConversationID) || item.SenderWorkerID != worker || !validID(item.RecipientWorkerID) || !utf8.ValidString(item.Preview) || utf8.RuneCountInString(item.Preview) > 160 || item.PreviewTruncated && utf8.RuneCountInString(item.Preview) != 160 || item.Acknowledged && !item.Seen || !validTimestamp(item.CreatedAt) || filter.Unread && item.Seen || filter.Unacknowledged && item.Acknowledged {
			return Page[InboxItem]{}, invalidResponse()
		}
	}
	return value, nil
}

// MessageDelivery reads one current receipt under the bound project authority.
// The message may be incoming, outgoing or between other project workers.
func (c *Client) MessageDelivery(ctx context.Context, id string) (MessageDelivery, error) {
	if !validID(id) {
		return MessageDelivery{}, validation()
	}
	var value MessageDelivery
	if err := c.get(ctx, c.projectPath("messages", id, "delivery"), nil, true, &value); err != nil {
		return MessageDelivery{}, err
	}
	if value.ID != id || !validID(value.SenderWorkerID) || !validID(value.RecipientWorkerID) || value.Acknowledged && !value.Seen {
		return MessageDelivery{}, invalidResponse()
	}
	return value, nil
}
