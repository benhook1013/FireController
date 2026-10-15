package mcpadapter

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
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
type inboxEventsInput struct {
	Cursor string `json:"cursor"`
	Limit  *int   `json:"limit,omitempty"`
	WaitMS *int   `json:"wait_ms,omitempty"`
}
type threadInput struct {
	ConversationID string `json:"conversation_id"`
	Limit          *int   `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}

func invalidArguments() *Diagnostic {
	return diagnostic("invalid_arguments", "Use only the declared tool fields with valid bounded values and required keys or revisions.", "Check this tool's input schema; the startup binding cannot be overridden.")
}

func parseArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	bound := 16 << 10
	if mutationTool(name) {
		bound = 256 << 10
	}
	if len(data) > bound || strictjson.Validate(data) != nil {
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
	if mutationTool(name) {
		return mutationArguments(name, data)
	}
	var limit *int
	var waitMS *int
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
	case "inbox_snapshot":
		dst = new(inboxInput)
	case "inbox_events":
		dst = new(inboxEventsInput)
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
	case *inboxEventsInput:
		limit, args.cursor, waitMS = v.Limit, v.Cursor, v.WaitMS
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
	if name == "inbox_events" {
		if _, ok := fields["cursor"]; !ok || !validEventCursor(args.cursor) {
			return args, invalidArguments()
		}
		if waitMS != nil {
			if *waitMS < 0 || *waitMS > 4000 {
				return args, invalidArguments()
			}
			args.waitMS = *waitMS
		}
	}
	if _, ok := fields["worker_id"]; ok && !validID(args.worker) {
		return args, invalidArguments()
	}
	if _, ok := fields["status"]; ok && !validStatus(args.status) {
		return args, invalidArguments()
	}
	if name == "job_read" || name == "job_history" || name == "inbox_thread" {
		if !validID(args.id) {
			return args, invalidArguments()
		}
	}
	return args, nil
}

func mutationTool(name string) bool {
	switch name {
	case "jobs_create", "job_edit", "inbox_send", "inbox_seen", "inbox_ack":
		return true
	}
	return false
}

func validKey(value string) bool {
	return client.ValidMutationKey(value)
}
func validStatus(value string) bool {
	return client.ValidStatus(value)
}
func validTitle(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= 240
}

// These transport guards run before client construction. The typed client and
// service still own authorization, normalization, receipts and mutation rules.
func mutationArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	switch name {
	case "jobs_create":
		var input struct {
			Key    string `json:"idempotency_key"`
			Title  string `json:"title"`
			Brief  string `json:"brief,omitempty"`
			Worker string `json:"worker_id,omitempty"`
			Status string `json:"status,omitempty"`
		}
		if strictjson.Decode(data, &input) != nil {
			return args, invalidArguments()
		}
		input.Title = strings.TrimSpace(input.Title)
		input.Worker = strings.TrimSpace(input.Worker)
		if input.Status == "" {
			input.Status = "active"
		}
		if !validTitle(input.Title) || !utf8.ValidString(input.Brief) || len(input.Brief) > 32<<10 || input.Worker != "" && !validID(input.Worker) || !validStatus(input.Status) {
			return args, invalidArguments()
		}
		args.key = input.Key
		args.create = client.CreateJobRequest{Title: input.Title, Brief: input.Brief, WorkerID: input.Worker, Status: input.Status}
	case "job_edit":
		var input struct {
			Key      string  `json:"idempotency_key"`
			ID       string  `json:"job_id"`
			Revision int64   `json:"expected_revision"`
			Title    *string `json:"title,omitempty"`
			Brief    *string `json:"brief,omitempty"`
			Worker   *string `json:"worker_id,omitempty"`
			Status   *string `json:"status,omitempty"`
		}
		if strictjson.Decode(data, &input) != nil || !validID(input.ID) || input.Revision < 1 || input.Revision == math.MaxInt64 || input.Title == nil && input.Brief == nil && input.Worker == nil && input.Status == nil {
			return args, invalidArguments()
		}
		if input.Title != nil {
			v := strings.TrimSpace(*input.Title)
			if !validTitle(v) {
				return args, invalidArguments()
			}
			input.Title = &v
		}
		if input.Brief != nil && (!utf8.ValidString(*input.Brief) || len(*input.Brief) > 32<<10) {
			return args, invalidArguments()
		}
		if input.Worker != nil {
			v := strings.TrimSpace(*input.Worker)
			if v != "" && !validID(v) {
				return args, invalidArguments()
			}
			input.Worker = &v
		}
		if input.Status != nil && !validStatus(*input.Status) {
			return args, invalidArguments()
		}
		args.key = input.Key
		args.id = input.ID
		args.edit = client.EditJobRequest{ExpectedRevision: input.Revision, Title: input.Title, Brief: input.Brief, WorkerID: input.Worker, Status: input.Status}
	case "inbox_send":
		var input struct {
			Key       string `json:"idempotency_key"`
			Recipient string `json:"recipient_worker_id"`
			Body      string `json:"body"`
			Reply     string `json:"reply_to_message_id,omitempty"`
		}
		if strictjson.Decode(data, &input) != nil || !validID(input.Recipient) || input.Reply != "" && !validID(input.Reply) || !utf8.ValidString(input.Body) || strings.TrimSpace(input.Body) == "" || len(input.Body) > 16<<10 {
			return args, invalidArguments()
		}
		args.key = input.Key
		args.send = client.SendMessageRequest{RecipientWorkerID: input.Recipient, Body: input.Body, ReplyToMessageID: input.Reply}
	case "inbox_seen", "inbox_ack":
		var input struct {
			Key string `json:"idempotency_key"`
			ID  string `json:"message_id"`
		}
		if strictjson.Decode(data, &input) != nil || !validID(input.ID) {
			return args, invalidArguments()
		}
		args.key = input.Key
		args.id = input.ID
	}
	if !validKey(args.key) {
		return args, invalidArguments()
	}
	return args, nil
}
func validID(value string) bool {
	return client.ValidID(value)
}

func validEventCursor(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
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
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit printable ASCII idempotency key; retain the same key and request after an unknown outcome. Never generated or automatically retried."}
	title := map[string]any{"type": "string", "minLength": 1, "maxLength": 240, "description": "Nonblank UTF-8 title, at most 240 bytes after trimming surrounding whitespace."}
	brief := map[string]any{"type": "string", "maxLength": 32 << 10, "description": "Explicit UTF-8 brief, at most 32 KiB; empty clears it on edit."}
	workerEdit := map[string]any{"type": "string", "maxLength": 128, "description": "Worker in the bound project; empty clears assignment."}
	workerCreate := map[string]any{"type": "string", "maxLength": 128, "description": "Worker in the bound project; omission or empty leaves the job unassigned."}
	schema := func(props map[string]any, required ...string) map[string]any {
		v := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			v["required"] = required
		}
		return v
	}
	editSchema := schema(map[string]any{"idempotency_key": key, "job_id": id, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "title": title, "brief": brief, "worker_id": workerEdit, "status": status}, "idempotency_key", "job_id", "expected_revision")
	editSchema["anyOf"] = []any{
		map[string]any{"required": []string{"title"}}, map[string]any{"required": []string{"brief"}},
		map[string]any{"required": []string{"worker_id"}}, map[string]any{"required": []string{"status"}},
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
		{"inbox_snapshot", "Read one bounded compact inbox page and its explicit scoped event cursor; no cursor is saved, no messages are marked seen or acknowledged, and no provider work occurs.", schema(map[string]any{"limit": limit, "cursor": cursor, "unread": map[string]any{"type": "boolean"}, "unacknowledged": map[string]any{"type": "boolean"}})},
		{"inbox_events", "Read one bounded incoming-message event page after an explicit cursor, optionally waiting up to 4000 ms. No cursor is saved, no automatic polling or subscription occurs, messages are never marked seen or acknowledged, and no provider work occurs.", schema(map[string]any{"cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Required explicit cursor from a scoped snapshot or replay page."}, "limit": limit, "wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 4000, "description": "One bounded wait; omission returns immediately."}}, "cursor")},
		{"inbox_thread", "Read one explicit conversation page with full immutable bodies; no receipt-state change.", schema(map[string]any{"conversation_id": id, "limit": limit, "cursor": cursor}, "conversation_id")},
		{"jobs_create", "Create a job in the bound project using an explicit key; return a compact receipt, without rereading brief/history.", schema(map[string]any{"idempotency_key": key, "title": title, "brief": brief, "worker_id": workerCreate, "status": status}, "idempotency_key", "title")},
		{"job_edit", "Edit selected job fields with an explicit key and expected revision; empty brief/worker clears those fields. No automatic retry.", editSchema},
		{"inbox_send", "Send an immutable message as the bound worker with an explicit key, optionally replying to a message; return a compact receipt.", schema(map[string]any{"idempotency_key": key, "recipient_worker_id": id, "body": map[string]any{"type": "string", "minLength": 1, "maxLength": 16 << 10}, "reply_to_message_id": id}, "idempotency_key", "recipient_worker_id", "body")},
		{"inbox_seen", "Explicitly mark a message seen as its bound recipient worker using the supplied key.", schema(map[string]any{"idempotency_key": key, "message_id": id}, "idempotency_key", "message_id")},
		{"inbox_ack", "Explicitly acknowledge a message as its bound recipient worker, also establishing seen, using the supplied key.", schema(map[string]any{"idempotency_key": key, "message_id": id}, "idempotency_key", "message_id")},
	}
}
