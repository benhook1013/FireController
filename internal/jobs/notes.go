package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const noteTimeFormat = "2006-01-02T15:04:05.000000000Z"
const maxNoteResponse = 128 << 10
const maxNotePageResponse = 512 << 10
const noteColumns = "id,project_id,COALESCE(worker_id,''),COALESCE(job_id,''),phase,kind,body,status,status_reason,revision,created_at,updated_at"
const noteRevisionColumns = "note_id,project_id,COALESCE(worker_id,''),COALESCE(job_id,''),phase,kind,body,status,status_reason,revision,created_at,updated_at,actor_id,actor_worker_id,recorded_at"

type note struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	WorkerID     string `json:"worker_id"`
	JobID        string `json:"job_id"`
	Phase        string `json:"phase"`
	Kind         string `json:"kind"`
	Body         string `json:"body"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}
type noteRevision struct {
	note
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	RecordedAt    string `json:"recorded_at"`
}
type noteSummary struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	Revision      int64  `json:"revision"`
	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	Phase         string `json:"phase"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	BodyPreview   string `json:"body_preview"`
	BodyTruncated bool   `json:"body_truncated"`
}
type noteHistorySummary struct {
	noteSummary
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	RecordedAt    string `json:"recorded_at"`
}
type noteCreateRequest struct {
	WorkerID *string `json:"worker_id"`
	JobID    *string `json:"job_id"`
	Phase    *string `json:"phase"`
	Kind     *string `json:"kind"`
	Body     *string `json:"body"`
}
type notePatchRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	WorkerID         *string `json:"worker_id,omitempty"`
	JobID            *string `json:"job_id,omitempty"`
	Phase            *string `json:"phase,omitempty"`
	Kind             *string `json:"kind,omitempty"`
	Body             *string `json:"body,omitempty"`
	Status           *string `json:"status,omitempty"`
	StatusReason     *string `json:"status_reason,omitempty"`
}

func (request *notePatchRequest) UnmarshalJSON(data []byte) error {
	type plain notePatchRequest
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return strictjson.ErrSyntax
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return strictjson.ErrSyntax
		}
	}
	var decoded plain
	if err := strictjson.Decode(data, &decoded); err != nil {
		return err
	}
	*request = notePatchRequest(decoded)
	return nil
}
func noteValidation() error {
	return core.Fail("validation_failed", "Supply a valid scoped note with bounded text, explicit fields and revision.", 400)
}
func noteUnavailable() error {
	return core.Fail("storage_unavailable", "Stored note history is unavailable.", 500)
}
func noteText(value string, limit int, nonblank, multiline bool) bool {
	return utf8.ValidString(value) && len(value) <= limit && (!nonblank || strings.TrimSpace(value) != "") && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && (!multiline || r != '\t' && r != '\n') }) < 0
}
func validNoteState(n note) bool {
	created, cErr := time.Parse(time.RFC3339Nano, n.CreatedAt)
	updated, uErr := time.Parse(time.RFC3339Nano, n.UpdatedAt)
	return validPrimaryID(n.ID) && validPrimaryID(n.ProjectID) && validNoteContent(n) && n.Revision >= 1 && cErr == nil && uErr == nil && !created.IsZero() && !updated.IsZero() && len(n.CreatedAt) <= 64 && len(n.UpdatedAt) <= 64
}
func validNoteContent(n note) bool {
	return (n.WorkerID != "" || n.JobID != "" || n.Phase != "") && (n.WorkerID == "" || validPrimaryID(n.WorkerID)) && (n.JobID == "" || validPrimaryID(n.JobID)) && noteText(n.Phase, 200, n.Phase != "", false) && (n.Kind == "reminder" || n.Kind == "source") && noteText(n.Body, 16<<10, true, true) && noteText(n.StatusReason, 1024, n.Status == "dismissed", true) && ((n.Status == "pending" || n.Status == "consumed") && n.StatusReason == "" || n.Status == "dismissed")
}
func validNoteRevision(n noteRevision) bool {
	return validNoteState(n.note) && validPrimaryID(n.ActorID) && (n.ActorWorkerID == "" || validPrimaryID(n.ActorWorkerID)) && n.RecordedAt == n.UpdatedAt
}
func noteDest(n *note) []any {
	return []any{&n.ID, &n.ProjectID, &n.WorkerID, &n.JobID, &n.Phase, &n.Kind, &n.Body, &n.Status, &n.StatusReason, &n.Revision, &n.CreatedAt, &n.UpdatedAt}
}
func scanNote(row interface{ Scan(...any) error }) (note, error) {
	var n note
	err := row.Scan(noteDest(&n)...)
	if err == nil && !validNoteState(n) {
		err = noteUnavailable()
	}
	return n, err
}
func scanNoteRevision(row interface{ Scan(...any) error }) (noteRevision, error) {
	var n noteRevision
	dest := append(noteDest(&n.note), &n.ActorID, &n.ActorWorkerID, &n.RecordedAt)
	err := row.Scan(dest...)
	if err == nil && !validNoteRevision(n) {
		err = noteUnavailable()
	}
	return n, err
}
func readNoteRevision(ctx context.Context, tx *sql.Tx, project, id string, revision int64) (noteRevision, error) {
	n, err := scanNoteRevision(tx.QueryRowContext(ctx, `SELECT `+noteRevisionColumns+` FROM note_revisions WHERE project_id=? AND note_id=? AND revision=?`, project, id, revision))
	if errors.Is(err, sql.ErrNoRows) {
		return n, core.Fail("note_not_found", "The note revision is unavailable in this project.", 404)
	}
	return n, err
}
func readNote(ctx context.Context, tx *sql.Tx, project, id string) (note, error) {
	n, err := scanNote(tx.QueryRowContext(ctx, `SELECT `+noteColumns+` FROM notes WHERE project_id=? AND id=?`, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, core.Fail("note_not_found", "The note is unavailable in this project.", 404)
	}
	if err != nil {
		return n, err
	}
	return n, nil
}
func checkNoteTargets(ctx context.Context, tx *sql.Tx, n note) error {
	if n.WorkerID != "" {
		if err := primaryWorker(ctx, tx, n.ProjectID, n.WorkerID); err != nil {
			return err
		}
	}
	if n.JobID != "" {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE project_id=? AND id=?`, n.ProjectID, n.JobID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Fail("job_not_found", "The target job is unavailable in this project.", 404)
		}
		return err
	}
	return nil
}
func appendNoteRevision(ctx context.Context, tx *sql.Tx, scope core.Scope, n note) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO note_revisions(project_id,note_id,revision,worker_id,job_id,phase,kind,body,status,status_reason,created_at,updated_at,actor_id,actor_worker_id,recorded_at) VALUES(?,?,?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?,?,?,?,?)`, n.ProjectID, n.ID, n.Revision, n.WorkerID, n.JobID, n.Phase, n.Kind, n.Body, n.Status, n.StatusReason, n.CreatedAt, n.UpdatedAt, scope.PrincipalID, scope.WorkerID, n.UpdatedAt)
	return err
}
func createNote(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req noteCreateRequest
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if req.WorkerID == nil || req.JobID == nil || req.Phase == nil || req.Kind == nil || req.Body == nil {
			core.WriteError(w, noteValidation())
			return
		}
		n := note{ProjectID: scope.ProjectID, WorkerID: *req.WorkerID, JobID: *req.JobID, Phase: *req.Phase, Kind: *req.Kind, Body: *req.Body, Status: "pending", Revision: 1}
		if !validNoteContent(n) {
			core.WriteError(w, noteValidation())
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "notes.create", Target: "notes", Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if err := checkNoteTargets(ctx, tx, n); err != nil {
				return nil, err
			}
			n.ID = core.NewID("note")
			n.CreatedAt = time.Now().UTC().Format(noteTimeFormat)
			n.UpdatedAt = n.CreatedAt
			_, err := tx.ExecContext(ctx, `INSERT INTO notes(project_id,id,worker_id,job_id,phase,kind,body,status,status_reason,revision,created_at,updated_at) VALUES(?,?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?,?,?)`, n.ProjectID, n.ID, n.WorkerID, n.JobID, n.Phase, n.Kind, n.Body, n.Status, n.StatusReason, n.Revision, n.CreatedAt, n.UpdatedAt)
			if err != nil {
				return nil, err
			}
			if err = appendNoteRevision(ctx, tx, scope, n); err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "note.created", n.ID, n.Revision, struct {
				NoteID string `json:"note_id"`
			}{n.ID}); err != nil {
				return nil, err
			}
			return receipt{ID: n.ID, Revision: n.Revision, Outcome: "created"}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 201, json.RawMessage(result))
	}
}
func patchNote(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		id := r.PathValue("note_id")
		if !validPrimaryID(id) {
			core.WriteError(w, noteValidation())
			return
		}
		var req notePatchRequest
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if req.ExpectedRevision == nil || *req.ExpectedRevision < 1 || *req.ExpectedRevision == math.MaxInt64 || req.WorkerID == nil && req.JobID == nil && req.Phase == nil && req.Kind == nil && req.Body == nil && req.Status == nil && req.StatusReason == nil {
			core.WriteError(w, noteValidation())
			return
		}
		// Validate supplied text before fingerprinting (JSON cannot preserve bad UTF-8).
		if req.WorkerID != nil && *req.WorkerID != "" && !validPrimaryID(*req.WorkerID) || req.JobID != nil && *req.JobID != "" && !validPrimaryID(*req.JobID) || req.Phase != nil && !noteText(*req.Phase, 200, *req.Phase != "", false) || req.Kind != nil && *req.Kind != "reminder" && *req.Kind != "source" || req.Body != nil && !noteText(*req.Body, 16<<10, true, true) || req.Status != nil && *req.Status != "pending" && *req.Status != "consumed" && *req.Status != "dismissed" || req.StatusReason != nil && !noteText(*req.StatusReason, 1024, false, true) || req.Status != nil && *req.Status == "dismissed" && (req.StatusReason == nil || strings.TrimSpace(*req.StatusReason) == "") {
			core.WriteError(w, noteValidation())
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "notes.patch", Target: id, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readNote(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			if current.Revision != *req.ExpectedRevision {
				return nil, &core.Error{Code: "note_revision_conflict", Message: "Reload the note before changing it.", Status: 409, Details: map[string]int64{"current_revision": current.Revision}}
			}
			next := current
			for _, field := range []struct {
				source *string
				target *string
			}{{req.WorkerID, &next.WorkerID}, {req.JobID, &next.JobID}, {req.Phase, &next.Phase}, {req.Kind, &next.Kind}, {req.Body, &next.Body}, {req.Status, &next.Status}, {req.StatusReason, &next.StatusReason}} {
				if field.source != nil {
					*field.target = *field.source
				}
			}
			if req.Status != nil && (*req.Status == "pending" || *req.Status == "consumed") && req.StatusReason == nil {
				next.StatusReason = ""
			}
			if !validNoteContent(next) {
				return nil, noteValidation()
			}
			if err = checkNoteTargets(ctx, tx, next); err != nil {
				return nil, err
			}
			if next == current {
				return receipt{ID: id, Revision: current.Revision, Outcome: "unchanged"}, nil
			}
			next.Revision++
			next.UpdatedAt = time.Now().UTC().Format(noteTimeFormat)
			_, err = tx.ExecContext(ctx, `UPDATE notes SET worker_id=NULLIF(?,''),job_id=NULLIF(?,''),phase=?,kind=?,body=?,status=?,status_reason=?,revision=?,updated_at=? WHERE project_id=? AND id=?`, next.WorkerID, next.JobID, next.Phase, next.Kind, next.Body, next.Status, next.StatusReason, next.Revision, next.UpdatedAt, next.ProjectID, next.ID)
			if err != nil {
				return nil, err
			}
			if err = appendNoteRevision(ctx, tx, scope, next); err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "note.updated", id, next.Revision, struct {
				NoteID string `json:"note_id"`
			}{id}); err != nil {
				return nil, err
			}
			return receipt{ID: id, Revision: next.Revision, Outcome: "updated"}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(result))
	}
}
func noteReadScope(store *core.Store, w http.ResponseWriter, r *http.Request) (core.Scope, string, bool) {
	scope, err := store.Scope(r, "read")
	if err == nil {
		_, err = core.QueryKeys(r, "invalid_query")
	}
	id := r.PathValue("note_id")
	if err == nil && !validPrimaryID(id) {
		err = noteValidation()
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, "", false
	}
	return scope, id, true
}
func getNote(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, id, ok := noteReadScope(store, w, r)
		if !ok {
			return
		}
		var n note
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			n, err = readNote(ctx, tx, scope.ProjectID, id)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, n, maxNoteResponse)
	}
}
func getNoteRevision(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, id, ok := noteReadScope(store, w, r)
		if !ok {
			return
		}
		revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != r.PathValue("revision") {
			core.WriteError(w, noteValidation())
			return
		}
		var n noteRevision
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readNote(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return err
			}
			n, err = readNoteRevision(ctx, tx, scope.ProjectID, id, revision)
			if err == nil && n.CreatedAt != current.CreatedAt {
				return noteUnavailable()
			}
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, n, maxNoteResponse)
	}
}
func compactNote(n note) noteSummary {
	preview := n.Body
	if len(preview) > 240 {
		preview = preview[:240]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
	}
	return noteSummary{ID: n.ID, ProjectID: n.ProjectID, Revision: n.Revision, WorkerID: n.WorkerID, JobID: n.JobID, Phase: n.Phase, Kind: n.Kind, Status: n.Status, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, BodyPreview: preview, BodyTruncated: len(preview) != len(n.Body)}
}

type notePage struct {
	Items   []noteSummary `json:"items"`
	Cursor  string        `json:"cursor,omitempty"`
	HasMore bool          `json:"has_more"`
}
type noteRevisionPage struct {
	Items   []noteHistorySummary `json:"items"`
	Cursor  string               `json:"cursor,omitempty"`
	HasMore bool                 `json:"has_more"`
}
type noteListFilter struct {
	Route, WorkerID, JobID, Phase, Kind, Status string
	IncludeAssignedJobs                         bool
}
type notePosition struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func listNotes(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		q, err := core.QueryKeys(r, "invalid_query", "worker_id", "job_id", "phase", "kind", "status", "include_assigned_jobs", "limit", "cursor")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := noteListFilter{Route: "notes-current", WorkerID: q.Get("worker_id"), JobID: q.Get("job_id"), Phase: q.Get("phase"), Kind: q.Get("kind"), Status: q.Get("status")}
		for _, key := range []string{"worker_id", "job_id", "phase", "kind", "status"} {
			if value, present := q[key]; present && value[0] == "" {
				core.WriteError(w, core.Fail("invalid_query", "Omit unused note filters rather than supplying empty values.", 400))
				return
			}
		}
		if filter.Status == "" {
			filter.Status = "pending"
		}
		assigned := q.Get("include_assigned_jobs")
		if _, present := q["include_assigned_jobs"]; present && assigned != "true" && assigned != "false" {
			core.WriteError(w, core.Fail("invalid_query", "Use true or false for assignment expansion.", 400))
			return
		}
		filter.IncludeAssignedJobs = assigned == "true"
		if filter.WorkerID != "" && !validPrimaryID(filter.WorkerID) || filter.JobID != "" && !validPrimaryID(filter.JobID) || !noteText(filter.Phase, 200, filter.Phase != "", false) || filter.Kind != "" && filter.Kind != "reminder" && filter.Kind != "source" || filter.Status != "pending" && filter.Status != "consumed" && filter.Status != "dismissed" && filter.Status != "all" || filter.IncludeAssignedJobs && filter.WorkerID == "" {
			core.WriteError(w, core.Fail("invalid_query", "Use valid exact note filters; assignment expansion requires a worker.", 400))
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filterBytes, _ := json.Marshal(filter)
		filterKey := string(filterBytes)
		after, err := store.ParseCursor(scope, filterKey, q.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var position notePosition
		if after != "" {
			if strictjson.Decode([]byte(after), &position) != nil || !validPrimaryID(position.ID) {
				core.WriteError(w, noteValidation())
				return
			}
			at, err := time.Parse(time.RFC3339Nano, position.CreatedAt)
			if err != nil || at.IsZero() || len(position.CreatedAt) > 64 {
				core.WriteError(w, noteValidation())
				return
			}
		}
		result := notePage{Items: make([]noteSummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			query := `SELECT ` + noteColumns + ` FROM notes WHERE project_id=?`
			args := []any{scope.ProjectID}
			if filter.WorkerID != "" {
				if filter.IncludeAssignedJobs {
					query += ` AND (worker_id=? OR EXISTS(SELECT 1 FROM jobs j WHERE j.project_id=notes.project_id AND j.id=notes.job_id AND j.worker_id=?))`
					args = append(args, filter.WorkerID, filter.WorkerID)
				} else {
					query += ` AND worker_id=?`
					args = append(args, filter.WorkerID)
				}
			}
			for _, field := range []struct{ column, value string }{{"job_id", filter.JobID}, {"phase", filter.Phase}, {"kind", filter.Kind}, {"status", filter.Status}} {
				if field.value != "" && !(field.column == "status" && field.value == "all") {
					query += ` AND ` + field.column + `=?`
					args = append(args, field.value)
				}
			}
			if after != "" {
				query += ` AND (created_at<? OR (created_at=? AND id<?))`
				args = append(args, position.CreatedAt, position.CreatedAt, position.ID)
			}
			query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			values := make([]note, 0, limit)
			for rows.Next() {
				n, err := scanNote(rows)
				if err != nil {
					rows.Close()
					return err
				}
				if len(values) == limit {
					result.HasMore = true
					break
				}
				values = append(values, n)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, n := range values {
				result.Items = append(result.Items, compactNote(n))
			}
			if result.HasMore {
				last := values[len(values)-1]
				data, _ := json.Marshal(notePosition{CreatedAt: last.CreatedAt, ID: last.ID})
				result.Cursor = store.Cursor(scope, filterKey, string(data))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, result, maxNotePageResponse)
	}
}
func noteHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query", "limit", "cursor"); err != nil {
			core.WriteError(w, err)
			return
		}
		id := r.PathValue("note_id")
		if !validPrimaryID(id) {
			core.WriteError(w, noteValidation())
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "notes-history:" + scope.ProjectID + ":" + id + ":revision-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := noteRevisionPage{Items: make([]noteHistorySummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readNote(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return err
			}
			query := `SELECT ` + noteRevisionColumns + ` FROM note_revisions WHERE project_id=? AND note_id=?`
			args := []any{scope.ProjectID, id}
			if before > 0 {
				query += ` AND revision<?`
				args = append(args, before)
			}
			query += ` ORDER BY revision DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				entry, err := scanNoteRevision(rows)
				if err != nil {
					return err
				}
				if entry.CreatedAt != current.CreatedAt || entry.Revision > current.Revision {
					return noteUnavailable()
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				result.Items = append(result.Items, noteHistorySummary{noteSummary: compactNote(entry.note), ActorID: entry.ActorID, ActorWorkerID: entry.ActorWorkerID, RecordedAt: entry.RecordedAt})
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, strconv.FormatInt(result.Items[len(result.Items)-1].Revision, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, result, maxNotePageResponse)
	}
}

// ValidateNotesSnapshot checks the complete notes domain in a read-only backup
// transaction. It does not authorize, migrate or mutate restored coordination.
func ValidateNotesSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+noteColumns+` FROM notes ORDER BY project_id,id`)
	if err != nil {
		return noteUnavailable()
	}
	// Stream notes; each selected history is ordered by its indexed identity.
	defer rows.Close()
	for rows.Next() {
		current, err := scanNote(rows)
		if err != nil {
			return noteUnavailable()
		}
		revisions, err := tx.QueryContext(ctx, `SELECT `+noteRevisionColumns+` FROM note_revisions WHERE project_id=? AND note_id=? ORDER BY revision`, current.ProjectID, current.ID)
		if err != nil {
			return noteUnavailable()
		}
		var count int64
		var previous noteRevision
		for revisions.Next() {
			entry, err := scanNoteRevision(revisions)
			if err != nil {
				revisions.Close()
				return noteUnavailable()
			}
			count++
			if entry.Revision != count || entry.CreatedAt != current.CreatedAt || count == 1 && (entry.Status != "pending" || entry.StatusReason != "" || entry.UpdatedAt != entry.CreatedAt) {
				revisions.Close()
				return noteUnavailable()
			}
			previous = entry
		}
		err = revisions.Err()
		revisions.Close()
		if err != nil || count != current.Revision || previous.note != current {
			return noteUnavailable()
		}
	}
	if rows.Err() != nil {
		return noteUnavailable()
	}
	// Foreign-key checks normally own orphans; retain this check for callers of
	// this domain validator that have not yet run the bundle-wide FK inspection.
	var orphan int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM note_revisions h WHERE NOT EXISTS(SELECT 1 FROM notes n WHERE n.project_id=h.project_id AND n.id=h.note_id) LIMIT 1`).Scan(&orphan)
	if !errors.Is(err, sql.ErrNoRows) {
		return noteUnavailable()
	}
	return nil
}
