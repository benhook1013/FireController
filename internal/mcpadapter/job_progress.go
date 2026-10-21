package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func jobProgressTool(name string) bool {
	switch name {
	case "job_update", "job_progress_history", "job_checklist_read", "job_checklist_replace", "job_checklist_history":
		return true
	}
	return false
}

func jobProgressArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	switch name {
	case "job_update":
		var fields map[string]json.RawMessage
		if strictjson.Validate(data) != nil || json.Unmarshal(data, &fields) != nil {
			return args, invalidArguments()
		}
		if json.Unmarshal(fields["job_id"], &args.id) != nil || json.Unmarshal(fields["idempotency_key"], &args.key) != nil || !validID(args.id) || !validKey(args.key) {
			return args, invalidArguments()
		}
		delete(fields, "job_id")
		delete(fields, "idempotency_key")
		body, _ := json.Marshal(fields)
		patch, err := client.DecodeJobPatch(body)
		if err != nil {
			return args, invalidArguments()
		}
		args.patch = patch
	case "job_checklist_replace":
		var in struct {
			ID                string          `json:"job_id"`
			Key               string          `json:"idempotency_key"`
			JobRevision       *int64          `json:"expected_job_revision"`
			ChecklistRevision *int64          `json:"expected_revision"`
			Items             json.RawMessage `json:"items"`
		}
		if strictjson.Decode(data, &in) != nil || !validID(in.ID) || !validKey(in.Key) || in.JobRevision == nil || in.ChecklistRevision == nil {
			return args, invalidArguments()
		}
		items, err := client.DecodeJobChecklistItems(in.Items)
		if err != nil {
			return args, invalidArguments()
		}
		args.checklist = client.ReplaceChecklistRequest{ExpectedJobRevision: *in.JobRevision, ExpectedRevision: *in.ChecklistRevision, Items: items}
		if client.ValidateJobChecklistRequest(args.checklist) != nil {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
	case "job_checklist_read":
		var in jobInput
		if strictjson.Decode(data, &in) != nil || !validID(in.JobID) {
			return args, invalidArguments()
		}
		args.id = in.JobID
	default:
		var in struct {
			JobID  string  `json:"job_id"`
			Limit  *int    `json:"limit,omitempty"`
			Cursor *string `json:"cursor,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || !validID(in.JobID) {
			return args, invalidArguments()
		}
		args.id, args.limit = in.JobID, 20
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

func jobProgressDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	revision := map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	cursor := map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
	text := func(max int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
	}
	item := schema(map[string]any{"id": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$"}, "text": text(1024), "done": map[string]any{"type": "boolean"}}, "id", "text", "done")
	return []definition{
		{"job_update", "Update selected job metadata and current progress with one revision guard and explicit key. Empty progress fields clear values; omitted members are preserved. Returns only a receipt, with no automatic retry.", jobUpdateSchema()},
		{"job_progress_history", "Read one explicit current-progress archive page, newest first; no automatic continuation or source-archive fanout.", schema(map[string]any{"job_id": id, "limit": limit, "cursor": cursor}, "job_id")},
		{"job_checklist_read", "Read the current ordered checklist and current job revision; an empty initial checklist has revision zero.", schema(map[string]any{"job_id": id}, "job_id")},
		{"job_checklist_replace", "Replace the ordered checklist using explicit job/checklist revision guards and key; stable item IDs preserve identity across completion, reopen and reorder. Does not edit the brief.", schema(map[string]any{"job_id": id, "idempotency_key": key, "expected_job_revision": revision, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "items": map[string]any{"type": "array", "maxItems": 100, "items": item}}, "job_id", "idempotency_key", "expected_job_revision", "expected_revision", "items")},
		{"job_checklist_history", "Read one explicit checklist revision archive page, newest first; full prior ordered states remain available after removals.", schema(map[string]any{"job_id": id, "limit": limit, "cursor": cursor}, "job_id")},
	}
}

func jobUpdateSchema() map[string]any {
	text := func(n int) map[string]any { return map[string]any{"type": "string", "maxLength": n} }
	progress := map[string]any{"type": "object", "additionalProperties": false, "minProperties": 1, "properties": map[string]any{"summary": text(8 << 10), "next": text(8 << 10), "blocker": text(4 << 10), "evidence": map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 1024}}}}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"job_id", "idempotency_key", "expected_revision"}, "properties": map[string]any{"job_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "title": map[string]any{"type": "string", "minLength": 1, "maxLength": 240}, "brief": text(32 << 10), "worker_id": text(128), "status": map[string]any{"type": "string", "enum": []string{"active", "parked", "blocked", "completed"}}, "progress": progress}}
	choices := []any{}
	for _, name := range []string{"title", "brief", "worker_id", "status", "progress"} {
		choices = append(choices, map[string]any{"required": []string{name}})
	}
	schema["anyOf"] = choices
	return schema
}
