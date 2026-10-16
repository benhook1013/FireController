package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// JobContextResult is one selected job and the bound worker's coordination from
// one authorized snapshot. Pending previews never replace full content reads.
type JobContextResult struct {
	ProjectID   string            `json:"project_id"`
	WorkerID    string            `json:"worker_id"`
	Job         Job               `json:"job"`
	Checkpoint  *Checkpoint       `json:"checkpoint"`
	Checklist   Checklist         `json:"checklist"`
	Primary     PrimaryJob        `json:"primary"`
	Pause       WorkerPause       `json:"pause"`
	Notes       Page[NoteSummary] `json:"notes"`
	Inbox       Page[InboxItem]   `json:"inbox"`
	EventCursor string            `json:"event_cursor"`
	Complete    bool              `json:"complete"`
	Omitted     []string          `json:"omitted"`
}

// The checkpoint key is required even when null. All other fields and nested
// false/zero/empty values retain the existing requiredShape contract.
func decodeJobContext(data []byte, dst any) error {
	out, ok := dst.(*JobContextResult)
	if !ok || strictjson.Validate(data) != nil {
		return strictjson.ErrSyntax
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 12 {
		return strictjson.ErrSyntax
	}
	for name, value := range map[string]any{"project_id": &out.ProjectID, "worker_id": &out.WorkerID, "job": &out.Job, "checklist": &out.Checklist, "primary": &out.Primary, "pause": &out.Pause, "notes": &out.Notes, "inbox": &out.Inbox, "event_cursor": &out.EventCursor, "complete": &out.Complete, "omitted": &out.Omitted} {
		raw, present := fields[name]
		if !present || decodeResponse(raw, value) != nil {
			return strictjson.ErrSyntax
		}
	}
	raw, present := fields["checkpoint"]
	if !present {
		return strictjson.ErrSyntax
	}
	out.Checkpoint = nil
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var checkpoint Checkpoint
		if decodeResponse(raw, &checkpoint) != nil {
			return strictjson.ErrSyntax
		}
		out.Checkpoint = &checkpoint
	}
	return nil
}

// JobContext performs one finite read and no section fanout or automatic
// continuation. A zero limit selects the documented default of twenty.
func (c *Client) JobContext(ctx context.Context, jobID string, limit int) (JobContextResult, error) {
	if !validID(jobID) || limit < 0 || limit > 100 {
		return JobContextResult{}, validation()
	}
	if limit == 0 {
		limit = 20
	}
	q, _ := pageQuery(PageOptions{Limit: limit})
	var out JobContextResult
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", jobID, "context"), q, true, "", nil, 200, false, &out, 1<<20, decodeJobContext); err != nil {
		return JobContextResult{}, err
	}
	if !c.validJobContext(out, jobID, limit) {
		return JobContextResult{}, invalidResponse()
	}
	return out, nil
}
func (c *Client) validJobContext(in JobContextResult, jobID string, limit int) bool {
	if in.ProjectID != c.identity.ProjectID || in.WorkerID != c.identity.WorkerID || in.Job.ID != jobID || !c.validJob(in.Job) || !validJobProgressTime(in.Job.CreatedAt) || !validJobProgressTime(in.Job.UpdatedAt) || !utf8.ValidString(in.Job.Brief) || !validOpaqueCursor(in.EventCursor) {
		return false
	}
	if in.Checkpoint != nil && (!c.validCheckpoint(*in.Checkpoint, jobID) || !validJobProgressTime(in.Checkpoint.CreatedAt) || in.Checkpoint.JobRevision > in.Job.Revision) {
		return false
	}
	if in.Checklist.JobID != jobID || in.Checklist.JobRevision != in.Job.Revision || in.Checklist.Revision < 0 || !validChecklistItems(in.Checklist.Items) || in.Checklist.Revision == 0 && len(in.Checklist.Items) != 0 {
		return false
	}
	if in.Primary.WorkerID != c.identity.WorkerID || in.Primary.Revision < 0 || in.Primary.JobID != "" && !validID(in.Primary.JobID) || in.Primary.Revision == 0 && in.Primary.JobID != "" {
		return false
	}
	if in.Pause.WorkerID != c.identity.WorkerID || in.Pause.Revision < 0 || !validWorkerPause(in.Pause.Paused, in.Pause.Reason) || in.Pause.Revision == 0 && in.Pause.Paused {
		return false
	}
	options := PageOptions{Limit: limit}
	if !validPage(in.Notes, options) || !validPage(in.Inbox, options) || in.Notes.Cursor != "" && !validOpaqueCursor(in.Notes.Cursor) || in.Inbox.Cursor != "" && !validOpaqueCursor(in.Inbox.Cursor) {
		return false
	}
	seen := map[string]bool{}
	var previous *NoteSummary
	for i := range in.Notes.Items {
		note := &in.Notes.Items[i]
		if !c.validNoteSummary(*note) || note.Status != "pending" || note.WorkerID != c.identity.WorkerID && note.JobID == "" || seen[note.ID] {
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
	seen = map[string]bool{}
	for _, message := range in.Inbox.Items {
		if !c.validInboxItem(message, InboxFilter{Unacknowledged: true}) || !validJobProgressTime(message.CreatedAt) || !utf8.ValidString(message.Preview) || message.Preview == "" || message.PreviewTruncated && utf8.RuneCountInString(message.Preview) != 160 || seen[message.ID] {
			return false
		}
		seen[message.ID] = true
	}
	expected := []string{}
	if len(in.Notes.Items) > 0 {
		expected = append(expected, "note_bodies")
	}
	if in.Notes.HasMore {
		expected = append(expected, "additional_notes")
	}
	if len(in.Inbox.Items) > 0 {
		expected = append(expected, "message_bodies")
	}
	if in.Inbox.HasMore {
		expected = append(expected, "additional_messages")
	}
	if in.Omitted == nil || len(in.Omitted) != len(expected) || in.Complete != (len(expected) == 0) {
		return false
	}
	for i, v := range expected {
		if in.Omitted[i] != v {
			return false
		}
	}
	return true
}
