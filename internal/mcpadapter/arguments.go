package mcpadapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type pageInput struct {
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}
type jobsInput struct {
	Limit    *int   `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	Status   string `json:"status,omitempty"`
}
type assignedInput struct {
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Status string `json:"status,omitempty"`
}
type jobInput struct {
	JobID string `json:"job_id"`
}
type historyInput struct {
	JobID  string `json:"job_id"`
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}
type inboxInput struct {
	Limit          *int   `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	Unread         bool   `json:"unread,omitempty"`
	Unacknowledged bool   `json:"unacknowledged,omitempty"`
}
type threadInput struct {
	ConversationID string `json:"conversation_id"`
	Limit          *int   `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}

func invalidArguments() *Diagnostic {
	return diagnostic("invalid_arguments", "Use only the declared tool fields with valid bounded IDs, filters and page options.", "Check this tool's input schema; the startup binding cannot be overridden.")
}

func parseArguments(name string, data json.RawMessage) (arguments, error) {
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
	var limit *int
	var dst any
	switch name {
	case "context", "doctor":
		dst = &struct{}{}
	case "workers_list":
		dst = new(pageInput)
	case "jobs_list":
		dst = new(jobsInput)
	case "jobs_assigned":
		dst = new(assignedInput)
	case "job_read":
		dst = new(jobInput)
	case "job_history":
		dst = new(historyInput)
	case "inbox_list":
		dst = new(inboxInput)
	case "inbox_thread":
		dst = new(threadInput)
	default:
		return args, invalidArguments()
	}
	if strictjson.Decode(data, dst) != nil {
		return args, invalidArguments()
	}
	switch v := dst.(type) {
	case *pageInput:
		limit, args.cursor = v.Limit, v.Cursor
	case *jobsInput:
		limit, args.cursor, args.worker, args.status = v.Limit, v.Cursor, v.WorkerID, v.Status
	case *assignedInput:
		limit, args.cursor, args.status = v.Limit, v.Cursor, v.Status
	case *jobInput:
		args.id = v.JobID
	case *historyInput:
		limit, args.cursor, args.id = v.Limit, v.Cursor, v.JobID
	case *inboxInput:
		limit, args.cursor, args.unread, args.unack = v.Limit, v.Cursor, v.Unread, v.Unacknowledged
	case *threadInput:
		limit, args.cursor, args.id = v.Limit, v.Cursor, v.ConversationID
	}
	if limit != nil {
		if *limit < 1 || *limit > 100 {
			return args, invalidArguments()
		}
		args.limit = *limit
	}
	if _, ok := fields["cursor"]; ok {
		if args.cursor == "" || len(args.cursor) > 4096 || !utf8.ValidString(args.cursor) {
			return args, invalidArguments()
		}
	}
	if _, ok := fields["worker_id"]; ok && !validID(args.worker) {
		return args, invalidArguments()
	}
	if _, ok := fields["status"]; ok && args.status != "active" && args.status != "parked" && args.status != "blocked" && args.status != "completed" {
		return args, invalidArguments()
	}
	if name == "job_read" || name == "job_history" || name == "inbox_thread" {
		if !validID(args.id) {
			return args, invalidArguments()
		}
	}
	return args, nil
}
func validID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\%?#") {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func toolDefinitions() []definition {
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Items in this explicit page; omission uses the service default."}
	cursor := map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Continuation cursor from this scoped read."}
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	status := map[string]any{"type": "string", "enum": []string{"active", "parked", "blocked", "completed"}}
	schema := func(props map[string]any, required ...string) map[string]any {
		v := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			v["required"] = required
		}
		return v
	}
	return []definition{
		{"context", "Inspect the immutable local profile configuration; no service verification or credential read.", schema(map[string]any{})},
		{"doctor", "Verify the configured service instance, project and worker without provider work.", schema(map[string]any{})},
		{"workers_list", "Read one compact worker page in the bound project.", schema(map[string]any{"limit": limit, "cursor": cursor})},
		{"jobs_list", "Read one compact job page with explicit optional worker/status filters.", schema(map[string]any{"limit": limit, "cursor": cursor, "worker_id": id, "status": status})},
		{"jobs_assigned", "Read one compact job page assigned to the bound worker; no primary/resume-context claim.", schema(map[string]any{"limit": limit, "cursor": cursor, "status": status})},
		{"job_read", "Read explicit full detail and brief for one job.", schema(map[string]any{"job_id": id}, "job_id")},
		{"job_history", "Read one explicit bounded full-state history page for a job.", schema(map[string]any{"job_id": id, "limit": limit, "cursor": cursor}, "job_id")},
		{"inbox_list", "Read one compact preview page for the bound worker; never mark seen or acknowledged.", schema(map[string]any{"limit": limit, "cursor": cursor, "unread": map[string]any{"type": "boolean"}, "unacknowledged": map[string]any{"type": "boolean"}})},
		{"inbox_thread", "Read one explicit conversation page with full immutable bodies; no receipt-state change.", schema(map[string]any{"conversation_id": id, "limit": limit, "cursor": cursor}, "conversation_id")},
	}
}
