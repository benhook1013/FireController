package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func primaryJobTool(name string) bool {
	return name == "job_primary_read" || name == "job_primary_replace" || name == "job_primary_history"
}

func primaryJobArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	switch name {
	case "job_primary_read":
		if strictjson.Decode(data, &struct{}{}) != nil {
			return args, invalidArguments()
		}
	case "job_primary_replace":
		var in struct {
			Key      string  `json:"idempotency_key"`
			Revision *int64  `json:"expected_revision"`
			JobID    *string `json:"job_id"`
		}
		if strictjson.Decode(data, &in) != nil || !validKey(in.Key) || in.Revision == nil || in.JobID == nil {
			return args, invalidArguments()
		}
		args.primary = client.ReplacePrimaryJobRequest{ExpectedRevision: *in.Revision, JobID: *in.JobID}
		if client.ValidatePrimaryJobRequest(args.primary) != nil {
			return args, invalidArguments()
		}
		args.key = in.Key
	case "job_primary_history":
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

func primaryJobDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return []definition{
		{"job_primary_read", "Read the bound worker's explicit primary job and independent focus revision; empty means no primary. Does not retrieve job detail or select work.", schema(map[string]any{})},
		{"job_primary_replace", "Select or clear the bound worker's primary job using its focus revision and an explicit idempotency key. Empty job_id clears; selecting requires a same-project job assigned to this worker. Does not change job status, brief or assignment. Retain the exact key and arguments after an uncertain response; no automatic retry.", schema(map[string]any{"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "job_id": map[string]any{"type": "string", "maxLength": 128}}, "idempotency_key", "expected_revision", "job_id")},
		{"job_primary_history", "Read one explicit newest-first focus-history page for the bound worker; no job detail, archive fanout or automatic pagination.", schema(map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}})},
	}
}
