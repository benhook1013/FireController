package client

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Note struct {
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

type NoteRevision struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	Phase         string `json:"phase"`
	Kind          string `json:"kind"`
	Body          string `json:"body"`
	Status        string `json:"status"`
	StatusReason  string `json:"status_reason"`
	Revision      int64  `json:"revision"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	RecordedAt    string `json:"recorded_at"`
}

type NoteSummary struct {
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

type NoteHistorySummary struct {
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
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	RecordedAt    string `json:"recorded_at"`
}

type CreateNoteRequest struct {
	WorkerID string `json:"worker_id"`
	JobID    string `json:"job_id"`
	Phase    string `json:"phase"`
	Kind     string `json:"kind"`
	Body     string `json:"body"`
}

type UpdateNoteRequest struct {
	ExpectedRevision int64   `json:"expected_revision"`
	WorkerID         *string `json:"worker_id,omitempty"`
	JobID            *string `json:"job_id,omitempty"`
	Phase            *string `json:"phase,omitempty"`
	Kind             *string `json:"kind,omitempty"`
	Body             *string `json:"body,omitempty"`
	Status           *string `json:"status,omitempty"`
	StatusReason     *string `json:"status_reason,omitempty"`
}

type NoteReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type NoteFilter struct {
	WorkerID            string
	JobID               string
	Phase               string
	Kind                string
	Status              string
	IncludeAssignedJobs bool
}

func noteText(value string, max int, required, multiline bool) bool {
	if !utf8.ValidString(value) || len(value) > max || required && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}
func noteScope(worker, job, phase string) bool {
	return (worker != "" || job != "" || phase != "") && (worker == "" || validID(worker)) && (job == "" || validID(job)) && noteText(phase, 200, phase != "", false)
}
func noteKind(kind string) bool { return kind == "reminder" || kind == "source" }
func noteStatus(status string) bool {
	return status == "pending" || status == "consumed" || status == "dismissed"
}

func ValidateCreateNoteRequest(in CreateNoteRequest) error {
	if !noteScope(in.WorkerID, in.JobID, in.Phase) || !noteKind(in.Kind) || !noteText(in.Body, 16<<10, true, true) {
		return mutationValidation()
	}
	return nil
}
func ValidateUpdateNoteRequest(in UpdateNoteRequest) error {
	if in.ExpectedRevision < 1 || in.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected note revision.", "Read the current note revision and supply it explicitly.", 400)
	}
	if in.WorkerID == nil && in.JobID == nil && in.Phase == nil && in.Kind == nil && in.Body == nil && in.Status == nil && in.StatusReason == nil {
		return mutationValidation()
	}
	if in.WorkerID != nil && *in.WorkerID != "" && !validID(*in.WorkerID) || in.JobID != nil && *in.JobID != "" && !validID(*in.JobID) || in.Phase != nil && !noteText(*in.Phase, 200, *in.Phase != "", false) || in.Kind != nil && !noteKind(*in.Kind) || in.Body != nil && !noteText(*in.Body, 16<<10, true, true) || in.Status != nil && !noteStatus(*in.Status) || in.StatusReason != nil && !noteText(*in.StatusReason, 1024, false, true) {
		return mutationValidation()
	}
	if in.WorkerID != nil && in.JobID != nil && in.Phase != nil && !noteScope(*in.WorkerID, *in.JobID, *in.Phase) {
		return mutationValidation()
	}
	if in.Status != nil {
		if *in.Status == "dismissed" {
			if in.StatusReason == nil || strings.TrimSpace(*in.StatusReason) == "" {
				return mutationValidation()
			}
		} else if in.StatusReason != nil && *in.StatusReason != "" {
			return mutationValidation()
		}
	}
	return nil
}
func ValidateNoteFilter(in NoteFilter) error {
	if in.WorkerID != "" && !validID(in.WorkerID) || in.JobID != "" && !validID(in.JobID) || !noteText(in.Phase, 200, in.Phase != "", false) || in.Kind != "" && !noteKind(in.Kind) || in.Status != "" && in.Status != "all" && !noteStatus(in.Status) || in.IncludeAssignedJobs && in.WorkerID == "" {
		return validation()
	}
	return nil
}
func (c *Client) CreateNote(ctx context.Context, key string, in CreateNoteRequest) (NoteReceipt, error) {
	if !validMutationKey(key) {
		return NoteReceipt{}, mutationValidation()
	}
	if err := ValidateCreateNoteRequest(in); err != nil {
		return NoteReceipt{}, err
	}
	var out NoteReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("notes"), key, in, http.StatusCreated, &out); err != nil {
		return NoteReceipt{}, err
	}
	if !validID(out.ID) || out.Revision != 1 || out.Outcome != "created" {
		return NoteReceipt{}, unknownMutation("invalid_response", "The service returned an invalid note receipt; its outcome is unknown.", 201)
	}
	return out, nil
}
func (c *Client) UpdateNote(ctx context.Context, key, id string, in UpdateNoteRequest) (NoteReceipt, error) {
	if !validMutationKey(key) || !validID(id) {
		return NoteReceipt{}, mutationValidation()
	}
	if err := ValidateUpdateNoteRequest(in); err != nil {
		return NoteReceipt{}, err
	}
	var out NoteReceipt
	if err := c.mutate(ctx, http.MethodPatch, c.projectPath("notes", id), key, in, 200, &out); err != nil {
		return NoteReceipt{}, err
	}
	if out.ID != id || !(out.Outcome == "updated" && out.Revision == in.ExpectedRevision+1 || out.Outcome == "unchanged" && out.Revision == in.ExpectedRevision) {
		return NoteReceipt{}, unknownMutation("invalid_response", "The service returned an invalid note receipt; its outcome is unknown.", 200)
	}
	return out, nil
}
func noteTimes(created, updated string) bool {
	a, e := time.Parse(time.RFC3339Nano, created)
	b, f := time.Parse(time.RFC3339Nano, updated)
	return e == nil && f == nil && !a.IsZero() && !b.IsZero() && strings.HasSuffix(created, "Z") && strings.HasSuffix(updated, "Z")
}
func (c *Client) validNote(in Note, id string) bool {
	return in.ID == id && validID(id) && in.ProjectID == c.identity.ProjectID && in.Revision >= 1 && (in.Revision != 1 || in.Status == "pending") && noteTimes(in.CreatedAt, in.UpdatedAt) && ValidateCreateNoteRequest(CreateNoteRequest{in.WorkerID, in.JobID, in.Phase, in.Kind, in.Body}) == nil && noteStatus(in.Status) && noteText(in.StatusReason, 1024, in.Status == "dismissed", true) && (in.Status == "dismissed" || in.StatusReason == "")
}
func (c *Client) Note(ctx context.Context, id string) (Note, error) {
	if !validID(id) {
		return Note{}, validation()
	}
	var out Note
	if err := c.requestBounded(ctx, "GET", c.projectPath("notes", id), nil, true, "", nil, 200, false, &out, 128<<10, decodeResponse); err != nil {
		return Note{}, err
	}
	if !c.validNote(out, id) {
		return Note{}, invalidResponse()
	}
	return out, nil
}
func noteActor(actor, worker, recorded, updated string) bool {
	return validID(actor) && (worker == "" || validID(worker)) && validJobProgressTime(recorded) && recorded == updated
}
func (c *Client) NoteRevision(ctx context.Context, id string, revision int64) (NoteRevision, error) {
	if !validID(id) || revision < 1 {
		return NoteRevision{}, validation()
	}
	var out NoteRevision
	if err := c.requestBounded(ctx, "GET", c.projectPath("notes", id, "history", strconv.FormatInt(revision, 10)), nil, true, "", nil, 200, false, &out, 128<<10, decodeResponse); err != nil {
		return NoteRevision{}, err
	}
	note := Note{out.ID, out.ProjectID, out.WorkerID, out.JobID, out.Phase, out.Kind, out.Body, out.Status, out.StatusReason, out.Revision, out.CreatedAt, out.UpdatedAt}
	if out.Revision != revision || !c.validNote(note, id) || !noteActor(out.ActorID, out.ActorWorkerID, out.RecordedAt, out.UpdatedAt) {
		return NoteRevision{}, invalidResponse()
	}
	return out, nil
}
func notePage(options PageOptions) (PageOptions, url.Values, error) {
	if options.Limit == 0 {
		options.Limit = 20
	}
	q, err := pageQuery(options)
	if err != nil || options.Cursor != "" && !validOpaqueCursor(options.Cursor) {
		return options, nil, validation()
	}
	return options, q, nil
}
func (c *Client) validNoteSummary(in NoteSummary) bool {
	return validID(in.ID) && in.ProjectID == c.identity.ProjectID && in.Revision >= 1 && (in.Revision != 1 || in.Status == "pending") && noteScope(in.WorkerID, in.JobID, in.Phase) && noteKind(in.Kind) && noteStatus(in.Status) && noteTimes(in.CreatedAt, in.UpdatedAt) && noteText(in.BodyPreview, 240, false, true) && in.BodyPreview != "" && (!in.BodyTruncated || len(in.BodyPreview) >= 237) && (in.BodyTruncated || strings.TrimSpace(in.BodyPreview) != "")
}
func (c *Client) Notes(ctx context.Context, options PageOptions, filter NoteFilter) (Page[NoteSummary], error) {
	options, q, err := notePage(options)
	if err != nil {
		return Page[NoteSummary]{}, err
	}
	if err = ValidateNoteFilter(filter); err != nil {
		return Page[NoteSummary]{}, err
	}
	for k, v := range map[string]string{"worker_id": filter.WorkerID, "job_id": filter.JobID, "phase": filter.Phase, "kind": filter.Kind, "status": filter.Status} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if filter.IncludeAssignedJobs {
		q.Set("include_assigned_jobs", "true")
	}
	var out Page[NoteSummary]
	if err = c.requestBounded(ctx, "GET", c.projectPath("notes"), q, true, "", nil, 200, false, &out, 512<<10, decodeResponse); err != nil {
		return Page[NoteSummary]{}, err
	}
	if !validPage(out, options) || out.Cursor != "" && !validOpaqueCursor(out.Cursor) {
		return Page[NoteSummary]{}, invalidResponse()
	}
	seen := map[string]bool{}
	var prev *NoteSummary
	for i := range out.Items {
		v := &out.Items[i]
		if !c.validNoteSummary(*v) || seen[v.ID] || filter.JobID != "" && v.JobID != filter.JobID || filter.Phase != "" && v.Phase != filter.Phase || filter.Kind != "" && v.Kind != filter.Kind || filter.Status != "all" && v.Status != defaultNoteStatus(filter.Status) || filter.JobID == "" && filter.WorkerID != "" && !filter.IncludeAssignedJobs && v.WorkerID != filter.WorkerID {
			return Page[NoteSummary]{}, invalidResponse()
		}
		if prev != nil {
			a, _ := time.Parse(time.RFC3339Nano, prev.CreatedAt)
			b, _ := time.Parse(time.RFC3339Nano, v.CreatedAt)
			if b.After(a) || b.Equal(a) && v.ID >= prev.ID {
				return Page[NoteSummary]{}, invalidResponse()
			}
		}
		seen[v.ID] = true
		prev = v
	}
	return out, nil
}
func defaultNoteStatus(value string) string {
	if value == "" {
		return "pending"
	}
	return value
}
func (c *Client) NoteHistory(ctx context.Context, id string, options PageOptions) (Page[NoteHistorySummary], error) {
	if !validID(id) {
		return Page[NoteHistorySummary]{}, validation()
	}
	options, q, err := notePage(options)
	if err != nil {
		return Page[NoteHistorySummary]{}, err
	}
	var out Page[NoteHistorySummary]
	if err = c.requestBounded(ctx, "GET", c.projectPath("notes", id, "history"), q, true, "", nil, 200, false, &out, 512<<10, decodeResponse); err != nil {
		return Page[NoteHistorySummary]{}, err
	}
	if !validPage(out, options) || out.Cursor != "" && !validOpaqueCursor(out.Cursor) {
		return Page[NoteHistorySummary]{}, invalidResponse()
	}
	previous := int64(0)
	for _, v := range out.Items {
		base := NoteSummary{v.ID, v.ProjectID, v.Revision, v.WorkerID, v.JobID, v.Phase, v.Kind, v.Status, v.CreatedAt, v.UpdatedAt, v.BodyPreview, v.BodyTruncated}
		if v.ID != id || !c.validNoteSummary(base) || !noteActor(v.ActorID, v.ActorWorkerID, v.RecordedAt, v.UpdatedAt) || previous != 0 && v.Revision >= previous {
			return Page[NoteHistorySummary]{}, invalidResponse()
		}
		previous = v.Revision
	}
	return out, nil
}
