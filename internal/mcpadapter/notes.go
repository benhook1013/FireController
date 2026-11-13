package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxNoteArgumentsBytes = 128 << 10

type noteToolCall struct {
	name, id, key             string
	revision                  int64
	create                    client.CreateNoteRequest
	update                    client.UpdateNoteRequest
	filter                    client.NoteFilter
	page                      client.PageOptions
	defaultWorker, allWorkers bool
}

func noteTool(name string) bool {
	switch name {
	case "note_create", "note_update", "note_read", "note_list", "note_history", "note_revision_read":
		return true
	}
	return false
}
func noteWrite(name string) bool { return name == "note_create" || name == "note_update" }
func noteArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > maxNoteArgumentsBytes {
		return args, &Diagnostic{Code: "invalid_arguments", Message: fmt.Sprintf("arguments exceed %d encoded JSON bytes (128 KiB), independently of the body limit of %d decoded UTF-8 bytes.", maxNoteArgumentsBytes, client.MaxNoteBodyBytes), Action: "Shorten the encoded payload and send only declared fields; omit unchanged fields in note_update. No request was sent.", Details: map[string]any{"field": "arguments", "max_encoded_bytes": maxNoteArgumentsBytes}}
	}
	if strictjson.Validate(data) != nil {
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
	n := &noteToolCall{name: name, page: client.PageOptions{Limit: 20}}
	args.notes = n
	switch name {
	case "note_create":
		var in struct {
			Key      string  `json:"idempotency_key"`
			WorkerID *string `json:"worker_id,omitempty"`
			JobID    string  `json:"job_id,omitempty"`
			Phase    string  `json:"phase,omitempty"`
			Kind     *string `json:"kind,omitempty"`
			Body     string  `json:"body"`
		}
		if strictjson.Decode(data, &in) != nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		n.key = in.Key
		n.defaultWorker = in.WorkerID == nil
		n.create = client.CreateNoteRequest{WorkerID: "bound-worker", JobID: in.JobID, Phase: in.Phase, Kind: "reminder", Body: in.Body}
		if in.WorkerID != nil {
			n.create.WorkerID = *in.WorkerID
		}
		if in.Kind != nil {
			n.create.Kind = *in.Kind
		}
		if err := client.ValidateCreateNoteRequest(n.create); err != nil {
			return args, noteValidationDiagnostic(err)
		}
	case "note_update":
		var in struct {
			Key              string  `json:"idempotency_key"`
			ID               string  `json:"note_id"`
			ExpectedRevision *int64  `json:"expected_revision"`
			WorkerID         *string `json:"worker_id,omitempty"`
			JobID            *string `json:"job_id,omitempty"`
			Phase            *string `json:"phase,omitempty"`
			Kind             *string `json:"kind,omitempty"`
			Body             *string `json:"body,omitempty"`
			Status           *string `json:"status,omitempty"`
			Reason           *string `json:"status_reason,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || !validKey(in.Key) || !validID(in.ID) || in.ExpectedRevision == nil {
			return args, invalidArguments()
		}
		n.key, n.id = in.Key, in.ID
		n.update = client.UpdateNoteRequest{ExpectedRevision: *in.ExpectedRevision, WorkerID: in.WorkerID, JobID: in.JobID, Phase: in.Phase, Kind: in.Kind, Body: in.Body, Status: in.Status, StatusReason: in.Reason}
		if err := client.ValidateUpdateNoteRequest(n.update); err != nil {
			return args, noteValidationDiagnostic(err)
		}
	case "note_read", "note_revision_read":
		if name == "note_read" {
			var in struct {
				ID string `json:"note_id"`
			}
			if strictjson.Decode(data, &in) != nil || !validID(in.ID) {
				return args, invalidArguments()
			}
			n.id = in.ID
		} else {
			var in struct {
				ID       string `json:"note_id"`
				Revision *int64 `json:"revision"`
			}
			if strictjson.Decode(data, &in) != nil || !validID(in.ID) || in.Revision == nil || *in.Revision < 1 {
				return args, invalidArguments()
			}
			n.id, n.revision = in.ID, *in.Revision
		}
	case "note_list":
		var in struct {
			WorkerID   *string `json:"worker_id,omitempty"`
			AllWorkers bool    `json:"all_workers,omitempty"`
			DirectOnly bool    `json:"direct_only,omitempty"`
			JobID      *string `json:"job_id,omitempty"`
			Phase      *string `json:"phase,omitempty"`
			Kind       *string `json:"kind,omitempty"`
			Status     *string `json:"status,omitempty"`
			Limit      *int    `json:"limit,omitempty"`
			Cursor     *string `json:"cursor,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.AllWorkers && (in.DirectOnly || in.WorkerID != nil) {
			return args, invalidArguments()
		}
		n.allWorkers = in.AllWorkers
		for ptr, dst := range map[*string]*string{in.JobID: &n.filter.JobID, in.Phase: &n.filter.Phase, in.Kind: &n.filter.Kind, in.Status: &n.filter.Status} {
			if ptr != nil {
				if *ptr == "" {
					return args, invalidArguments()
				}
				*dst = *ptr
			}
		}
		if !n.allWorkers {
			n.defaultWorker = in.WorkerID == nil
			n.filter.WorkerID = "bound-worker"
			if in.WorkerID != nil {
				if *in.WorkerID == "" {
					return args, invalidArguments()
				}
				n.filter.WorkerID = *in.WorkerID
			}
			n.filter.IncludeAssignedJobs = !in.DirectOnly
		}
		if client.ValidateNoteFilter(n.filter) != nil || !noteToolPage(&n.page, in.Limit, in.Cursor) {
			return args, invalidArguments()
		}
	case "note_history":
		var in struct {
			ID     string  `json:"note_id"`
			Limit  *int    `json:"limit,omitempty"`
			Cursor *string `json:"cursor,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || !validID(in.ID) || !noteToolPage(&n.page, in.Limit, in.Cursor) {
			return args, invalidArguments()
		}
		n.id = in.ID
	default:
		return args, invalidArguments()
	}
	return args, nil
}
func noteValidationDiagnostic(err error) *Diagnostic {
	var failure *client.Error
	if errors.As(err, &failure) {
		if details, ok := failure.Details.(map[string]any); ok && details["field"] == "body" {
			return &Diagnostic{Code: "invalid_arguments", Message: failure.Message, Action: failure.Action, Details: failure.Details}
		}
	}
	return invalidArguments()
}

func noteToolPage(p *client.PageOptions, limit *int, cursor *string) bool {
	if limit != nil {
		if *limit < 1 || *limit > 100 {
			return false
		}
		p.Limit = *limit
	}
	if cursor != nil {
		if !validEventCursor(*cursor) {
			return false
		}
		p.Cursor = *cursor
	}
	return true
}
func executeNoteTool(ctx context.Context, c *client.Client, n *noteToolCall) (any, error) {
	switch n.name {
	case "note_create":
		in := n.create
		if n.defaultWorker {
			in.WorkerID = c.Identity().WorkerID
		}
		return c.CreateNote(ctx, n.key, in)
	case "note_update":
		return c.UpdateNote(ctx, n.key, n.id, n.update)
	case "note_read":
		return c.Note(ctx, n.id)
	case "note_revision_read":
		return c.NoteRevision(ctx, n.id, n.revision)
	case "note_history":
		return c.NoteHistory(ctx, n.id, n.page)
	default:
		filter := n.filter
		if n.defaultWorker {
			filter.WorkerID = c.Identity().WorkerID
		}
		return c.Notes(ctx, n.page, filter)
	}
}
func noteDefinitions() []definition {
	str := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	id := func() map[string]any { return map[string]any{"type": "string", "minLength": 1, "maxLength": 128} }
	schema := func(properties map[string]any, required ...string) map[string]any {
		out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	kind := map[string]any{"type": "string", "enum": []string{"reminder", "source"}}
	status := map[string]any{"type": "string", "enum": []string{"pending", "consumed", "dismissed"}}
	page := func() map[string]any {
		return map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}}
	}
	body := func() map[string]any {
		v := str(client.MaxNoteBodyBytes)
		v["description"] = fmt.Sprintf("Nonblank UTF-8 text, at most %d decoded UTF-8 bytes (16 KiB), not characters or encoded JSON bytes.", client.MaxNoteBodyBytes)
		return v
	}
	create := map[string]any{"idempotency_key": id(), "worker_id": str(128), "job_id": str(128), "phase": str(200), "kind": kind, "body": body()}
	update := map[string]any{"idempotency_key": id(), "note_id": id(), "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "worker_id": str(128), "job_id": str(128), "phase": str(200), "kind": kind, "body": body(), "status": status, "status_reason": str(1024)}
	list := page()
	list["worker_id"] = id()
	list["all_workers"], list["direct_only"] = map[string]any{"type": "boolean"}, map[string]any{"type": "boolean"}
	list["job_id"], list["phase"], list["kind"], list["status"] = id(), str(200), kind, map[string]any{"type": "string", "enum": []string{"pending", "consumed", "dismissed", "all"}}
	history := page()
	history["note_id"] = id()
	return []definition{
		{"note_create", "Create one private project note. Worker defaults to the bound worker; otherwise target an existing same-project worker or job. Phase is an optional label, not a target. Kind defaults to reminder. Body is exact nonblank text up to 16384 decoded UTF-8 bytes (16 KiB), not characters or encoded JSON bytes; MCP arguments are limited to 131072 encoded JSON bytes (128 KiB); preserve the original key and arguments after an uncertain response. No job, message, pause or provider side effect.", schema(create, "idempotency_key", "body")},
		{"note_update", "Patch only supplied note fields with an explicit revision and original idempotency key. Body is limited to 16384 decoded UTF-8 bytes (16 KiB), not characters or encoded JSON bytes; MCP arguments are limited to 131072 encoded JSON bytes (128 KiB). Omitted fields are preserved; empty scope strings clear selectors, but an explicit scope change must leave a worker or job target. Retained phase-only notes remain readable and can be changed by body/status without a scope change. Dismissal needs a nonblank status_reason; pending/consumed clears old reason. A status change needs no body echo. After an uncertain response, retry the exact original key and arguments; no automatic retry.", schema(update, "idempotency_key", "note_id", "expected_revision")},
		{"note_read", "Read one full current note without consuming it or loading history.", schema(map[string]any{"note_id": id()}, "note_id")},
		{"note_list", "Read one compact note page, pending by default. Default scope is the bound worker plus currently assigned jobs; explicit worker_id selects that project worker and its assigned jobs. direct_only disables assignment expansion, all_workers selects project scope. Do not combine all_workers with worker_id or direct_only. Explicit job_id selects all notes for that exact job, independent of worker defaults/assignment, and continues job_read note cursors directly. Other exact filters combine with AND. Default 20, maximum 100, no automatic pagination, full bodies or reasons.", schema(list)},
		{"note_history", "Read one compact newest-revision-first note history page with actor attribution, default 20, maximum 100. No full bodies/reasons or automatic followup.", schema(history, "note_id")},
		{"note_revision_read", "Read one full immutable note revision with actor attribution.", schema(map[string]any{"note_id": id(), "revision": map[string]any{"type": "integer", "minimum": 1}}, "note_id", "revision")},
	}
}
