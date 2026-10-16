package mcpadapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

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
	if strictjson.Decode(data, &in) != nil || in.Cursor == "" || len(in.Cursor) > 4096 || !utf8.ValidString(in.Cursor) || in.Cursor != strings.TrimSpace(in.Cursor) || strings.IndexFunc(in.Cursor, unicode.IsControl) >= 0 {
		return args, invalidArguments()
	}
	args.cursor, args.limit, args.messageWaitSeconds = in.Cursor, 20, 25
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return args, invalidArguments()
		}
		args.limit = *in.Limit
	}
	if in.WaitSeconds != nil {
		if *in.WaitSeconds < 1 || *in.WaitSeconds > 60 {
			return args, invalidArguments()
		}
		args.messageWaitSeconds = *in.WaitSeconds
	}
	return args, nil
}

func messageWaitDefinition() definition {
	return definition{"wait_for_messages", "Wait for new incoming-message events after the bound-worker cursor; one compact page or timeout (default 25s, max 60s). Bodies require conversation reads. No acknowledgments, provider work or auto-reconnect. Background delivery depends on host.", map[string]any{"type": "object", "properties": map[string]any{"cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "wait_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60}}, "required": []string{"cursor"}, "additionalProperties": false}}
}
