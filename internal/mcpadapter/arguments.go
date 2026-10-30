package mcpadapter

import (
	"bytes"
	"encoding/json"
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
	Newest         bool   `json:"newest,omitempty"`
	Unacknowledged *bool  `json:"unacknowledged,omitempty"`
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
type messageInput struct {
	MessageID string `json:"message_id"`
}

func invalidArguments() *Diagnostic {
	return diagnostic("invalid_arguments", "Use only the declared tool fields with valid bounded values and required keys or revisions.", "Check this tool's input schema and correct the request. No request was sent.")
}

func parseArguments(name string, data json.RawMessage) (arguments, error) {
	if existingManagementTool(name) {
		var a arguments
		var err error
		a.existingManagement, err = parseExistingManagementArguments(name, data)
		return a, err
	}
	if ownerManagementTool(name) {
		return ownerManagementArguments(name, data)
	}
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if name == "inbox_list" || name == "outbox_list" {
		cleaned, worker, ok := selectedWorkerArguments(data)
		if !ok {
			return args, invalidArguments()
		}
		data, args.worker = cleaned, worker
	}
	if reviewBatchTool(name) {
		return reviewBatchArguments(name, data)
	}
	if subagentTool(name) {
		return subagentArguments(name, data)
	}
	bound := 16 << 10
	if mutationTool(name) {
		bound = 256 << 10
	}
	if name == "job_update" {
		// Tool selectors and the key sit outside the public PATCH body.
		bound = client.JobPatchMaxEncodedBytes + 1024
	}
	if name == "job_update" {
		if len(data) > bound {
			return args, jobUpdateDiagnostic("job_update", "The encoded patch exceeds its 384 KiB body allowance plus selectors. Brief and progress limits count UTF-8 bytes.")
		}
		return jobProgressArguments(name, data)
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
	if name == "review_queue_add" || name == "review_queue_move" || name == "review_queue_remove" {
		return queueChangeArguments(name, data)
	}
	if name == "review_queue" {
		options, active, err := reviewActiveQueueArguments(data)
		args.limit, args.cursor, args.reviewActiveQueue = options.Limit, options.Cursor, active
		return args, err
	}
	if name == "job_revision_read" {
		return jobRevisionArguments(data)
	}
	if name == "job_status_read" {
		return jobStatusArguments(data)
	}
	if name == "review_operation_read" {
		input, err := operationStatusArguments(data)
		if err != nil {
			return args, err
		}
		args.operationStatus, args.reviewSourceWait = input, input.WaitSeconds
		return args, nil
	}
	if checkpointTool(name) {
		return checkpointArguments(name, data)
	}
	if primaryJobTool(name) {
		return primaryJobArguments(name, data)
	}
	if name == "job_summary_read" || name == "job_summary_set" {
		return jobSummaryArguments(name, data)
	}
	if jobProgressTool(name) {
		return jobProgressArguments(name, data)
	}
	if name == "review_start" || name == "review_selection_read" {
		return reviewRunArguments(name, data)
	}
	if name == "review_rounds_read" || name == "review_rounds_configure" {
		return roundControlArguments(name, data)
	}
	if name == "review_operations_list" {
		return operationListArguments(data)
	}
	if reviewControlTool(name) {
		return reviewControlArguments(name, data)
	}
	if mutationTool(name) {
		return mutationArguments(name, data)
	}
	if reviewTool(name) {
		return reviewArguments(name, data, fields)
	}
	var limit *int
	var waitMS *int
	var dst any
	switch name {
	case "context", "doctor", "presentation_read":
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
	case "inbox_list", "outbox_list":
		dst = new(inboxInput)
	case "inbox_snapshot":
		dst = new(inboxInput)
	case "inbox_events":
		dst = new(inboxEventsInput)
	case "inbox_thread":
		dst = new(threadInput)
	case "inbox_message_read", "inbox_delivery_read":
		dst = new(messageInput)
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
		if _, supplied := fields["status"]; !supplied {
			args.status = "open"
		} else if v.Status == "all" {
			args.status = ""
		} else if v.Status == "" {
			return args, invalidArguments()
		}
	case *jobInput:
		args.id = v.JobID
	case *historyInput:
		limit, args.cursor, args.id = v.Limit, v.Cursor, v.JobID
	case *inboxInput:
		args.newest = v.Newest
		limit, args.cursor, args.unread, args.unack = v.Limit, v.Cursor, v.Unread, true
		if v.Unacknowledged != nil {
			args.unack = *v.Unacknowledged
		}
	case *inboxEventsInput:
		limit, args.cursor, waitMS = v.Limit, v.Cursor, v.WaitMS
	case *threadInput:
		limit, args.cursor, args.id = v.Limit, v.Cursor, v.ConversationID
	case *messageInput:
		args.id = v.MessageID
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
	if _, ok := fields["status"]; ok && !client.ValidJobStatusFilter(args.status) && !(name == "jobs_assigned" && args.status == "") {
		return args, invalidArguments()
	}
	if name == "job_read" || name == "job_history" || name == "inbox_thread" || name == "inbox_message_read" || name == "inbox_delivery_read" {
		if !validID(args.id) {
			return args, invalidArguments()
		}
	}
	return args, nil
}

func mutationTool(name string) bool {
	if existingManagementWrite(name) {
		return true
	}
	if ownerManagementWrite(name) {
		return true
	}
	if subagentWrite(name) {
		return true
	}
	if reviewBatchTool(name) {
		return true
	}
	if name == "review_queue_add" || name == "review_queue_move" || name == "review_queue_remove" || name == "review_start" || name == "review_rounds_configure" {
		return true
	}
	if name == "job_summary_set" || name == "job_primary_replace" || name == "job_update" || name == "job_checklist_replace" || name == "job_checklist_item_update" {
		return true
	}
	switch name {
	case "review_checkpoint_publish", "review_checkpoint_recover", "review_finding_route", "review_route_decide", "review_route_fix", "jobs_create", "inbox_send", "inbox_seen", "inbox_ack", "review_finding_decide", "review_finding_fix", "review_source_finalize":
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
	statusFilter := map[string]any{"type": "string", "enum": []string{"active", "parked", "blocked", "completed", "open"}, "description": "Stored status or the list-only open filter, which selects active, blocked and parked jobs in that priority order."}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit printable ASCII idempotency key; retain the same key and request after an unknown outcome. Never generated or automatically retried."}
	title := map[string]any{"type": "string", "minLength": 1, "maxLength": 240, "description": "Nonblank UTF-8 title, at most 240 bytes after trimming surrounding whitespace."}
	brief := map[string]any{"type": "string", "maxLength": 32 << 10, "description": "Explicit UTF-8 brief, at most 32 KiB; empty clears it on edit."}
	workerCreate := map[string]any{"type": "string", "maxLength": 128, "description": "Worker in the bound project; omission or empty leaves the job unassigned."}
	schema := func(props map[string]any, required ...string) map[string]any {
		v := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			v["required"] = required
		}
		return v
	}
	return append([]definition{
		{"presentation_read", "Read the bound project appearance and metadata watermark explicitly. No ordinary job or inbox call includes settings; no provider work or mutation.", schema(map[string]any{})},
		{"context", "Inspect the immutable local profile and running client source revision/state; no service verification or credential read.", schema(map[string]any{})},
		{"doctor", "Verify the configured instance/project/worker and return current worker_pause (revision, paused and private reason), plus running client source revision/state. No provider work or pause changes.", schema(map[string]any{})},
		{"workers_list", "Read one compact worker page in the bound project.", schema(map[string]any{"limit": limit, "cursor": cursor})},
		{"jobs_list", "Read one compact job page with explicit optional worker/status filters.", schema(map[string]any{"limit": limit, "cursor": cursor, "worker_id": id, "status": statusFilter})},
		{"jobs_assigned", "Read one compact assigned-job page, default open (active, blocked, parked). Explicit status:all includes completed history; no primary/resume-context claim.", schema(map[string]any{"limit": limit, "cursor": cursor, "status": map[string]any{"type": "string", "enum": []string{"open", "active", "blocked", "parked", "completed", "all"}, "default": "open", "description": "Defaults to open; all explicitly includes completed jobs."}})},
		{"job_read", "Read one current job snapshot: full private brief and progress, remaining checklist, and exact-job pending-note previews. No worker inbox. Continue pending job notes with note_list(job_id,all_workers:true,cursor); discover worker reminders separately with note_list().", schema(map[string]any{"job_id": id}, "job_id")},
		{"job_history", "Read one explicit bounded full-state history page for a job.", schema(map[string]any{"job_id": id, "limit": limit, "cursor": cursor}, "job_id")},
		{"outbox_list", "Read sent handoffs for the bound sender with recipients and current seen/acknowledged state. Defaults to unacknowledged; explicit unacknowledged:false includes handled history. Read-only, bounded; no recipient impersonation or receipt writes.", schema(map[string]any{"worker_id": id, "limit": limit, "cursor": cursor, "unread": map[string]any{"type": "boolean"}, "newest": map[string]any{"type": "boolean", "description": "Optional newest-first insertion order; retain on continuation. Does not mark older pending messages handled."}, "unacknowledged": map[string]any{"type": "boolean", "default": true, "description": "Defaults to true: pending messages, including seen-but-unhandled. Explicit false includes acknowledged history."}})},
		{"inbox_list", "Read one compact pending-message page for the selected worker (defaults to the bound worker) (unacknowledged by default, including seen-but-unhandled). Set unacknowledged:false for history. Never marks seen or acknowledged.", schema(map[string]any{"worker_id": id, "limit": limit, "cursor": cursor, "unread": map[string]any{"type": "boolean"}, "newest": map[string]any{"type": "boolean", "description": "Optional newest-first insertion order; retain on continuation. Does not mark older pending messages handled."}, "unacknowledged": map[string]any{"type": "boolean", "default": true, "description": "Defaults to true: pending messages, including seen-but-unhandled. Explicit false includes acknowledged history."}})},
		{"inbox_snapshot", "Read one bounded pending inbox page (unacknowledged by default; explicit false includes history) and its explicit scoped event cursor; no cursor is saved, no messages are marked seen or acknowledged, and no provider work occurs.", schema(map[string]any{"limit": limit, "cursor": cursor, "unread": map[string]any{"type": "boolean"}, "newest": map[string]any{"type": "boolean", "description": "Optional newest-first insertion order; retain on continuation. Does not mark older pending messages handled."}, "unacknowledged": map[string]any{"type": "boolean", "default": true, "description": "Defaults to true: pending messages, including seen-but-unhandled. Explicit false includes acknowledged history."}})},
		{"inbox_events", "Read one bounded incoming-message event page after an explicit cursor, optionally waiting up to 4000 ms. No cursor is saved, no automatic polling or subscription occurs, messages are never marked seen or acknowledged, and no provider work occurs.", schema(map[string]any{"cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Required explicit cursor from a scoped snapshot or replay page."}, "limit": limit, "wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 4000, "description": "One bounded wait; omission returns immediately."}}, "cursor")},
		{"inbox_thread", "Read one explicit conversation page with full immutable bodies; no receipt-state change.", schema(map[string]any{"conversation_id": id, "limit": limit, "cursor": cursor}, "conversation_id")},
		{"inbox_delivery_read", "Read current recipient seen/acknowledged state for one selected message in the bound project, without its body or another worker inbox. Never changes receipt state.", schema(map[string]any{"message_id": id}, "message_id")},
		{"inbox_message_read", "Read one selected full immutable message by ID without paging older conversation bodies. Does not mark seen or acknowledged; expand conversation context explicitly when needed.", schema(map[string]any{"message_id": id}, "message_id")},
		{"jobs_create", "Create a job in the bound project using an explicit key; return a compact receipt, without rereading brief/history.", schema(map[string]any{"idempotency_key": key, "title": title, "brief": brief, "worker_id": workerCreate, "status": status}, "idempotency_key", "title")},
		{"inbox_send", "Send an immutable message as the bound worker with an explicit key, optionally replying to a message; return a compact receipt.", schema(map[string]any{"idempotency_key": key, "recipient_worker_id": id, "body": map[string]any{"type": "string", "minLength": 1, "maxLength": 16 << 10}, "reply_to_message_id": id}, "idempotency_key", "recipient_worker_id", "body")},
		{"inbox_seen", "Explicitly mark a message seen as its bound recipient worker using the supplied key.", schema(map[string]any{"idempotency_key": key, "message_id": id}, "idempotency_key", "message_id")},
		{"inbox_ack", "Explicitly acknowledge a message as its bound recipient worker, also establishing seen, using the supplied key.", schema(map[string]any{"idempotency_key": key, "message_id": id}, "idempotency_key", "message_id")},
	}, reviewDefinitions()...)
}
