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
	if len(data) > 8<<10 || strictjson.Validate(data) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, invalidArguments()
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
	if client.ValidateMessageWait(args.cursor, args.limit, args.messageWaitSeconds) != nil || args.messageWaitSeconds > maxToolWaitSeconds {
		return args, invalidArguments()
	}
	return args, nil
}

func messageWaitDefinition() definition {
	return definition{"wait_for_messages", fmt.Sprintf("Wait for new incoming-message events after the bound-worker cursor; one compact page or timeout (default %ds, max %ds). Use inbox_message_read for one full body, or inbox_thread for conversation context. No acknowledgments, provider work or auto-reconnect. Background delivery depends on host.", client.MessageWaitDefaultSeconds, maxToolWaitSeconds), map[string]any{"type": "object", "properties": map[string]any{"cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": client.MessageWaitMaxLimit}, "wait_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxToolWaitSeconds}}, "required": []string{"cursor"}, "additionalProperties": false}}
}
