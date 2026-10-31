package mcpadapter

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func messageWaitArguments(data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > 8<<10 || strictjson.Validate(data) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	var cursor string
	if raw, present := fields["cursor"]; !present || json.Unmarshal(raw, &cursor) != nil || cursor == "" {
		err := diagnostic("invalid_arguments", "cursor is required and must be a nonempty incoming-message event cursor.", "Supply inbox_snapshot.event_cursor or the cursor from a successful wait. If the saved cursor was lost, read a fresh inbox_snapshot and handle its pending messages. No request was sent.")
		err.Details = map[string]string{"field": "cursor"}
		return args, err
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return args, invalidArguments()
		}
	}
	var in struct {
		Cursor      string `json:"cursor"`
		Limit       *int   `json:"limit,omitempty"`
		WaitSeconds *int   `json:"wait_seconds,omitempty"`
	}
	if strictjson.Decode(data, &in) != nil {
		return args, invalidArguments()
	}
	args.cursor, args.limit, args.messageWaitSeconds = in.Cursor, client.MessageWaitDefaultLimit, client.MessageWaitDefaultSeconds
	if in.Limit != nil {
		args.limit = *in.Limit
	}
	if in.WaitSeconds != nil {
		args.messageWaitSeconds = *in.WaitSeconds
	}
	if args.messageWaitSeconds < 1 || args.messageWaitSeconds > maxToolWaitSeconds {
		err := diagnostic("invalid_arguments", fmt.Sprintf("wait_seconds must be from 1 to %d seconds for wait_for_messages.", maxToolWaitSeconds), fmt.Sprintf("Use 1–%d seconds (default %d), or inbox_snapshot for an immediate inbox read. No request was sent.", maxToolWaitSeconds, client.MessageWaitDefaultSeconds))
		err.Details = map[string]any{"field": "wait_seconds", "minimum": 1, "maximum": maxToolWaitSeconds}
		return args, err
	}
	if client.ValidateMessageWait(args.cursor, args.limit, args.messageWaitSeconds) != nil {
		return args, invalidArguments()
	}
	return args, nil
}

func messageWaitDefinition() definition {
	return definition{"wait_for_messages", fmt.Sprintf("Wait for new incoming-message events after the bound-worker cursor; one compact page or timeout (1–%ds, default %ds). Use inbox_message_read for one full body, or inbox_thread for conversation context. No acknowledgments, provider work or auto-reconnect. Background delivery depends on host.", maxToolWaitSeconds, client.MessageWaitDefaultSeconds), map[string]any{"type": "object", "properties": map[string]any{"cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Required incoming-message event cursor from inbox_snapshot.event_cursor or a successful wait; inbox.cursor is pagination, not an event cursor."}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": client.MessageWaitMaxLimit}, "wait_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxToolWaitSeconds, "default": client.MessageWaitDefaultSeconds}}, "required": []string{"cursor"}, "additionalProperties": false}}
}
