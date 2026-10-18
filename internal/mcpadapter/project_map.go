package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type projectMapToolCall struct {
	name, id, key string
	revision      int64
	entry         client.ProjectMapEditable
	association   client.ReplaceJobWorkstreamRequest
	filter        client.ProjectMapFilter
	page          client.PageOptions
}

func projectMapTool(name string) bool {
	switch name {
	case "map_entry_create", "map_entry_replace", "map_entry_list", "map_entry_read", "map_entry_history", "map_entry_revision_read", "job_workstream_read", "job_workstream_replace", "job_workstream_history":
		return true
	}
	return false
}
func projectMapWrite(name string) bool {
	return name == "map_entry_create" || name == "map_entry_replace" || name == "job_workstream_replace"
}
func projectMapArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > 128<<10 || strictjson.Validate(data) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	allowed := map[string]bool{}
	var required []string
	switch name {
	case "map_entry_create":
		required = []string{"idempotency_key", "entry"}
	case "map_entry_replace":
		required = []string{"idempotency_key", "entry_id", "expected_revision", "entry"}
	case "map_entry_read":
		required = []string{"entry_id"}
	case "map_entry_revision_read":
		required = []string{"entry_id", "revision"}
	case "map_entry_history":
		required = []string{"entry_id"}
		allowed["limit"], allowed["cursor"] = true, true
	case "map_entry_list":
		allowed["kind"], allowed["archived"], allowed["limit"], allowed["cursor"] = true, true, true, true
	case "job_workstream_read":
		required = []string{"job_id"}
	case "job_workstream_replace":
		required = []string{"job_id", "expected_revision", "workstream_id", "idempotency_key"}
	case "job_workstream_history":
		required = []string{"job_id"}
		allowed["limit"], allowed["cursor"] = true, true
	default:
		return args, invalidArguments()
	}
	for _, key := range required {
		allowed[key] = true
		if _, ok := fields[key]; !ok {
			return args, invalidArguments()
		}
	}
	for key, raw := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return args, invalidArguments()
		}
	}
	var in struct {
		EntryID          string                     `json:"entry_id"`
		JobID            string                     `json:"job_id"`
		Key              string                     `json:"idempotency_key"`
		Entry            *client.ProjectMapEditable `json:"entry"`
		ExpectedRevision *int64                     `json:"expected_revision"`
		Revision         *int64                     `json:"revision"`
		WorkstreamID     *string                    `json:"workstream_id"`
		Kind             *string                    `json:"kind"`
		Archived         *bool                      `json:"archived"`
		Limit            *int                       `json:"limit"`
		Cursor           *string                    `json:"cursor"`
	}
	if strictjson.Decode(data, &in) != nil {
		return args, invalidArguments()
	}
	m := &projectMapToolCall{name: name, key: in.Key, page: client.PageOptions{Limit: 25}}
	args.projectMap = m
	if _, ok := fields["entry_id"]; ok {
		if !validID(in.EntryID) {
			return args, invalidArguments()
		}
		m.id = in.EntryID
	}
	if _, ok := fields["job_id"]; ok {
		if !validID(in.JobID) {
			return args, invalidArguments()
		}
		m.id = in.JobID
	}
	if projectMapWrite(name) && !validKey(in.Key) {
		return args, invalidArguments()
	}
	if raw, ok := fields["entry"]; ok {
		var err error
		m.entry, err = client.DecodeProjectMapEditable(raw)
		if err != nil {
			return args, invalidArguments()
		}
	}
	if in.ExpectedRevision != nil {
		m.revision = *in.ExpectedRevision
	}
	if name == "map_entry_replace" && client.ValidateReplaceProjectMapEntryRequest(client.ReplaceProjectMapEntryRequest{ExpectedRevision: m.revision, Entry: m.entry}) != nil {
		return args, invalidArguments()
	}
	if name == "job_workstream_replace" {
		m.association = client.ReplaceJobWorkstreamRequest{ExpectedRevision: m.revision, WorkstreamID: *in.WorkstreamID}
		if client.ValidateReplaceJobWorkstreamRequest(m.association) != nil {
			return args, invalidArguments()
		}
	}
	if in.Revision != nil {
		if *in.Revision < 1 {
			return args, invalidArguments()
		}
		m.revision = *in.Revision
	}
	if in.Kind != nil {
		if *in.Kind != "workstream" && *in.Kind != "return_point" && *in.Kind != "section" {
			return args, invalidArguments()
		}
		m.filter.Kind = *in.Kind
	}
	if in.Archived != nil {
		m.filter.Archived = *in.Archived
	}
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return args, invalidArguments()
		}
		m.page.Limit = *in.Limit
	}
	if in.Cursor != nil {
		if !validEventCursor(*in.Cursor) {
			return args, invalidArguments()
		}
		m.page.Cursor = *in.Cursor
	}
	return args, nil
}
func executeProjectMapTool(ctx context.Context, c *client.Client, m *projectMapToolCall) (any, error) {
	switch m.name {
	case "map_entry_create":
		return c.CreateProjectMapEntry(ctx, m.key, m.entry)
	case "map_entry_replace":
		return c.ReplaceProjectMapEntry(ctx, m.key, m.id, client.ReplaceProjectMapEntryRequest{ExpectedRevision: m.revision, Entry: m.entry})
	case "map_entry_read":
		return c.ProjectMapEntry(ctx, m.id)
	case "map_entry_list":
		return c.ProjectMapEntries(ctx, m.page, m.filter)
	case "map_entry_history":
		return c.ProjectMapHistory(ctx, m.id, m.page)
	case "map_entry_revision_read":
		return c.ProjectMapRevision(ctx, m.id, m.revision)
	case "job_workstream_read":
		return c.JobWorkstream(ctx, m.id)
	case "job_workstream_replace":
		return c.ReplaceJobWorkstream(ctx, m.key, m.id, m.association)
	default:
		return c.JobWorkstreamHistory(ctx, m.id, m.page)
	}
}
func projectMapDefinitions() []definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		value := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			value["required"] = required
		}
		return value
	}
	text := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	revision := map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}
	phase := object(map[string]any{"id": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_-]{0,63}$"}, "name": text(160), "state": text(240), "archived": map[string]any{"type": "boolean"}}, "id", "name")
	link := object(map[string]any{"label": text(120), "url": text(2048)}, "label", "url")
	entry := object(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"workstream", "return_point", "section"}}, "title": text(240), "description": text(8192), "state": text(240), "where": text(8192), "next": text(8192), "trigger": text(8192), "phases": map[string]any{"type": []string{"array", "null"}, "items": phase, "maxItems": 64}, "links": map[string]any{"type": []string{"array", "null"}, "items": link, "maxItems": 12}, "archived": map[string]any{"type": "boolean"}, "position": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}}, "kind", "title")
	pages := func(extra map[string]any) map[string]any {
		extra["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		extra["cursor"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
		return extra
	}
	return []definition{
		{"map_entry_create", "Create one project-scoped section, workstream or return point. Prose supports safe browser Markdown; position selects editorial order. Entry is bounded text, at most 64 KiB encoded; kind/title are required and omitted optional fields default empty. No provider work. Retain the exact key and request after an uncertain result.", object(map[string]any{"idempotency_key": id, "entry": entry}, "idempotency_key", "entry")},
		{"map_entry_replace", "Replace the complete editable map entry using its expected revision; omitted optional fields clear/default. Keep every previously introduced phase ID, optionally archived. Does not change job status or provider state. Retain the exact key/request after uncertainty.", object(map[string]any{"idempotency_key": id, "entry_id": id, "expected_revision": revision, "entry": entry}, "idempotency_key", "entry_id", "expected_revision", "entry")},
		{"map_entry_list", "Read one compact map-entry page, default 25 active entries. Optional exact kind/archived filters; no bodies, automatic pagination or provider reads.", object(pages(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"workstream", "return_point", "section"}}, "archived": map[string]any{"type": "boolean"}}))},
		{"map_entry_read", "Read one complete current map entry in the bound project.", object(map[string]any{"entry_id": id}, "entry_id")},
		{"map_entry_history", "Read one newest-first compact map revision page, default 25; select full historical content explicitly.", object(pages(map[string]any{"entry_id": id}), "entry_id")},
		{"map_entry_revision_read", "Read one full captured map revision with its actor and time.", object(map[string]any{"entry_id": id, "revision": map[string]any{"type": "integer", "minimum": 1}}, "entry_id", "revision")},
		{"job_workstream_read", "Read the job's independent workstream association, initially revision zero; no full map fetch or job-status change.", object(map[string]any{"job_id": id}, "job_id")},
		{"job_workstream_replace", "Replace the job's independent workstream association using an explicit revision/key. Empty workstream_id clears it; a new selection must be an active same-project workstream. Does not edit the brief or job status.", object(map[string]any{"job_id": id, "idempotency_key": id, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "workstream_id": text(128)}, "job_id", "idempotency_key", "expected_revision", "workstream_id")},
		{"job_workstream_history", "Read one captured workstream-association history page, default 25, without automatic pagination or full entry content.", object(pages(map[string]any{"job_id": id}), "job_id")},
	}
}
