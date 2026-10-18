package mcpadapter

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func workerPauseTool(name string) bool {
	return name == "worker_pause_read" || name == "worker_pause_replace" || name == "worker_pause_history"
}

func workerPauseArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > 16<<10 || strictjson.Validate(data) != nil {
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
	switch name {
	case "worker_pause_read":
		if strictjson.Decode(data, &struct{}{}) != nil {
			return args, invalidArguments()
		}
	case "worker_pause_replace":
		var in struct {
			Key      string  `json:"idempotency_key"`
			Revision *int64  `json:"expected_revision"`
			Paused   *bool   `json:"paused"`
			Reason   *string `json:"reason"`
		}
		if strictjson.Decode(data, &in) != nil || !validKey(in.Key) || in.Revision == nil || in.Paused == nil || in.Reason == nil {
			return args, invalidArguments()
		}
		args.pause = client.ReplaceWorkerPauseRequest{ExpectedRevision: *in.Revision, Paused: *in.Paused, Reason: *in.Reason}
		if client.ValidateWorkerPauseRequest(args.pause) != nil {
			return args, invalidArguments()
		}
		args.key = in.Key
	case "worker_pause_history":
		var in struct {
			Limit  *int    `json:"limit,omitempty"`
			Cursor *string `json:"cursor,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil {
			return args, invalidArguments()
		}
		args.limit = 20
		if in.Limit != nil {
			if *in.Limit < 1 || *in.Limit > 100 {
				return args, invalidArguments()
			}
			args.limit = *in.Limit
		}
		if in.Cursor != nil {
			if !validEventCursor(*in.Cursor) {
				return args, invalidArguments()
			}
			args.cursor = *in.Cursor
		}
	}
	return args, nil
}

func workerPauseDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return []definition{
		{"worker_pause_read", "Read the bound worker's explicit coordination pause and independent revision. This does not report presence or stop execution.", schema(map[string]any{})},
		{"worker_pause_replace", "Replace the bound worker's coordination pause using an explicit revision and idempotency key. Reason is exact UTF-8 text of at most 1024 bytes; only TAB and newline controls are allowed. An unpaused state requires empty reason. Does not stop agents or change jobs, messages, focus or review execution. Retain the original key and arguments after an uncertain response; no automatic retry.", schema(map[string]any{"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "paused": map[string]any{"type": "boolean"}, "reason": map[string]any{"type": "string", "maxLength": 1024}}, "idempotency_key", "expected_revision", "paused", "reason")},
		{"worker_pause_history", "Read one explicit newest-first pause-history page for the bound worker, without automatic pagination or job reads.", schema(map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}})},
	}
}
