package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// Progress is the complete current progress, distinct from a public summary.
type Progress struct {
	Summary  string   `json:"summary"`
	Next     string   `json:"next"`
	Blocker  string   `json:"blocker"`
	Evidence []string `json:"evidence"`
}

// ProgressPatch preserves omitted members; explicit empty values clear them.
type ProgressPatch struct {
	Summary  *string   `json:"summary,omitempty"`
	Next     *string   `json:"next,omitempty"`
	Blocker  *string   `json:"blocker,omitempty"`
	Evidence *[]string `json:"evidence,omitempty"`
}
type JobChecklistSummary struct {
	Revision  int64           `json:"revision"`
	Remaining []ChecklistItem `json:"remaining"`
	Completed int             `json:"completed"`
	Total     int             `json:"total"`
}
type ProgressHistoryEntry struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Progress    Progress `json:"progress"`
	CreatedAt   string   `json:"created_at"`
}

func validProgress(p Progress) bool {
	if p.Evidence == nil || !checkpointText(p.Summary, 8<<10, false) || !checkpointText(p.Next, 8<<10, false) || !checkpointText(p.Blocker, 4<<10, false) || len(p.Evidence) > 10 {
		return false
	}
	n := len(p.Summary) + len(p.Next) + len(p.Blocker)
	for _, ref := range p.Evidence {
		if !checkpointText(ref, 1024, true) {
			return false
		}
		n += len(ref)
	}
	return n <= 16<<10
}
func normalizeJobPatch(p UpdateJobRequest) (UpdateJobRequest, error) {
	if p.ExpectedRevision < 1 || p.ExpectedRevision == math.MaxInt64 {
		return p, fail("invalid_revision", "A positive writable job revision is required.", "Read the current job and supply its revision.", 400)
	}
	if p.Title == nil && p.Brief == nil && p.WorkerID == nil && p.Status == nil && p.Progress == nil {
		return p, mutationValidation()
	}
	if p.Title != nil {
		v := strings.TrimSpace(*p.Title)
		if !utf8.ValidString(v) || v == "" || len(v) > 240 {
			return p, mutationValidation()
		}
		p.Title = &v
	}
	if p.Brief != nil && (!utf8.ValidString(*p.Brief) || len(*p.Brief) > 32<<10) {
		return p, mutationValidation()
	}
	if p.WorkerID != nil {
		v := strings.TrimSpace(*p.WorkerID)
		if v != "" && !validID(v) {
			return p, mutationValidation()
		}
		p.WorkerID = &v
	}
	if p.Status != nil && !validStatus(*p.Status) {
		return p, mutationValidation()
	}
	if p.Progress != nil {
		v := *p.Progress
		if v.Summary == nil && v.Next == nil && v.Blocker == nil && v.Evidence == nil {
			return p, mutationValidation()
		}
		full := Progress{Evidence: []string{}}
		if v.Summary != nil {
			full.Summary = *v.Summary
		}
		if v.Next != nil {
			full.Next = *v.Next
		}
		if v.Blocker != nil {
			full.Blocker = *v.Blocker
		}
		if v.Evidence != nil {
			if *v.Evidence == nil {
				return p, mutationValidation()
			}
			full.Evidence = *v.Evidence
		}
		if !validProgress(full) {
			return p, mutationValidation()
		}
	}
	return p, nil
}

// ValidateJobPatch checks only supplied fields; the service validates the merged state.
func ValidateJobPatch(p UpdateJobRequest) error { _, err := normalizeJobPatch(p); return err }

// DecodeJobPatch admits only declared supplied fields, without loading credentials.
func DecodeJobPatch(data []byte) (UpdateJobRequest, error) {
	var p UpdateJobRequest
	if len(data) > 256<<10 || strictjson.Decode(data, &p) != nil || !nonnullJobPatch(data) {
		return p, mutationValidation()
	}
	_, err := normalizeJobPatch(p)
	return p, err
}
func nonnullJobPatch(data []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return false
	}
	for name, raw := range fields {
		if strings.TrimSpace(string(raw)) == "null" {
			return false
		}
		if name == "progress" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil || nested == nil {
				return false
			}
			for _, r := range nested {
				if strings.TrimSpace(string(r)) == "null" {
					return false
				}
			}
		}
	}
	return true
}
func (c *Client) JobProgressHistory(ctx context.Context, id string, options PageOptions) (Page[ProgressHistoryEntry], error) {
	if options.Limit == 0 {
		options.Limit = 20
	}
	query, err := pageQuery(options)
	if err != nil || !validID(id) {
		return Page[ProgressHistoryEntry]{}, validation()
	}
	var page Page[ProgressHistoryEntry]
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "progress", "history"), query, true, "", nil, 200, false, &page, 4<<20, decodeResponse); err != nil {
		return Page[ProgressHistoryEntry]{}, err
	}
	if !validPage(page, options) {
		return Page[ProgressHistoryEntry]{}, invalidResponse()
	}
	seen := map[string]bool{}
	var prior int64
	for _, v := range page.Items {
		if !validID(v.ID) || seen[v.ID] || v.ProjectID != c.identity.ProjectID || v.JobID != id || v.JobRevision < 1 || v.Sequence < 1 || prior != 0 && v.Sequence >= prior || !validID(v.ActorID) || v.WorkerID != "" && !validID(v.WorkerID) || !validProgress(v.Progress) || !validJobProgressTime(v.CreatedAt) {
			return Page[ProgressHistoryEntry]{}, invalidResponse()
		}
		seen[v.ID] = true
		prior = v.Sequence
	}
	return page, nil
}
func validProgressPreview(text string, truncated bool) bool {
	return utf8.ValidString(text) && checkpointText(text, 240, false) && (!truncated || len(text) >= 237)
}

func (c *Client) validJobCurrent(v Job) bool {
	if !validProgress(v.Progress) || !validJobProgressTime(v.CreatedAt) || !validJobProgressTime(v.UpdatedAt) || (!utf8.ValidString(v.Brief) || len(v.Brief) > 32<<10) {
		return false
	}
	check := v.Checklist
	if check.Revision < 0 || check.Completed < 0 || check.Total > 100 || check.Total != check.Completed+len(check.Remaining) || !validChecklistItems(check.Remaining) || check.Revision == 0 && check.Total != 0 {
		return false
	}
	for _, item := range check.Remaining {
		if item.Done {
			return false
		}
	}
	if !validPage(v.Notes, PageOptions{Limit: 5}) || v.Notes.Cursor != "" && !validOpaqueCursor(v.Notes.Cursor) {
		return false
	}
	seen := map[string]bool{}
	var previous *NoteSummary
	for i := range v.Notes.Items {
		note := &v.Notes.Items[i]
		if !c.validNoteSummary(*note) || note.JobID != v.ID || note.Status != "pending" || seen[note.ID] {
			return false
		}
		if previous != nil {
			a, _ := time.Parse(time.RFC3339Nano, previous.CreatedAt)
			b, _ := time.Parse(time.RFC3339Nano, note.CreatedAt)
			if b.After(a) || b.Equal(a) && note.ID >= previous.ID {
				return false
			}
		}
		seen[note.ID] = true
		previous = note
	}
	return true
}
func (c *Client) validJobState(v JobState) bool {
	return validID(v.ID) && v.ProjectID == c.identity.ProjectID && cleanText(v.Title, 240) && utf8.ValidString(v.Brief) && len(v.Brief) <= 32<<10 && (v.WorkerID == "" || validID(v.WorkerID)) && validStatus(v.Status) && v.Revision > 0 && validJobProgressTime(v.CreatedAt) && validJobProgressTime(v.UpdatedAt) && (v.Progress == nil || validProgress(*v.Progress))
}
