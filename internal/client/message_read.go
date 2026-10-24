package client

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Message retrieves one immutable message in the bound project. Reading does not
// mark it seen or acknowledged and never loads other conversation messages.
func (c *Client) Message(ctx context.Context, id string) (Message, error) {
	if !validID(id) {
		return Message{}, validation()
	}
	var value Message
	if err := c.get(ctx, c.projectPath("messages", id), nil, true, &value); err != nil {
		return Message{}, err
	}
	if value.ID != id || !validID(value.ConversationID) || !validID(value.SenderWorkerID) || !validID(value.RecipientWorkerID) || value.ReplyToMessageID != "" && (!validID(value.ReplyToMessageID) || value.ReplyToMessageID == id) || !utf8.ValidString(value.Body) || strings.TrimSpace(value.Body) == "" || len(value.Body) > 16<<10 || !validTimestamp(value.CreatedAt) {
		return Message{}, invalidResponse()
	}
	return value, nil
}
