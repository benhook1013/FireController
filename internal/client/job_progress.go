package client

import (
	"context"
	"math"
	"net/http"
	"time"
)

type updateArchiveContent struct {
	ExpectedJobRevision int64    `json:"expected_job_revision"`
	Body                string   `json:"body"`
	Evidence            []string `json:"evidence"`
}

type Update struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Body        string   `json:"body"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}

type ChecklistItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

type Checklist struct {
	JobID       string          `json:"job_id"`
	JobRevision int64           `json:"job_revision"`
	Revision    int64           `json:"revision"`
	Items       []ChecklistItem `json:"items"`
}

type ReplaceChecklistRequest struct {
	ExpectedJobRevision int64           `json:"expected_job_revision"`
	ExpectedRevision    int64           `json:"expected_revision"`
	Items               []ChecklistItem `json:"items"`
}

type ChecklistReceipt struct {
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}

type ChecklistHistoryEntry struct {
	Revision    int64           `json:"revision"`
	JobRevision int64           `json:"job_revision"`
	ActorID     string          `json:"actor_id"`
	WorkerID    string          `json:"worker_id"`
	Items       []ChecklistItem `json:"items"`
	CreatedAt   string          `json:"created_at"`
}

func validateUpdateArchiveContent(request updateArchiveContent) error {
	if request.ExpectedJobRevision < 1 {
		return fail("invalid_revision", "A positive expected job revision is required.", "Read the current job revision and supply it explicitly.", 400)
	}
	if !checkpointText(request.Body, 8<<10, true) || len(request.Evidence) > 10 {
		return mutationValidation()
	}
	size := len(request.Body)
	for _, reference := range request.Evidence {
		if !checkpointText(reference, 1024, true) {
			return mutationValidation()
		}
		size += len(reference)
	}
	if size > 16<<10 {
		return mutationValidation()
	}
	return nil
}

func validChecklistItems(items []ChecklistItem) bool {
	if items == nil || len(items) > 100 {
		return false
	}
	seen := make(map[string]bool, len(items))
	size := 0
	for _, item := range items {
		if len(item.ID) < 1 || len(item.ID) > 128 || seen[item.ID] || !checkpointText(item.Text, 1024, true) {
			return false
		}
		for _, ch := range []byte(item.ID) {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
				return false
			}
		}
		seen[item.ID] = true
		size += len(item.ID) + len(item.Text)
		if size > 16<<10 {
			return false
		}
	}
	return true
}

// ValidateJobChecklistRequest preserves explicit empty lists and rejects an
// omitted list. Job revisions are compared; checklist revisions may increment.
func ValidateJobChecklistRequest(request ReplaceChecklistRequest) error {
	if request.ExpectedJobRevision < 1 || request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a positive job revision and a writable checklist revision.", "Read the current job and checklist; supply their revisions explicitly.", 400)
	}
	if !validChecklistItems(request.Items) {
		return mutationValidation()
	}
	return nil
}

// DecodeJobChecklistItems admits the complete explicit item shape, including a
// present boolean done value, before adapters load credentials or do service I/O.
func DecodeJobChecklistItems(data []byte) ([]ChecklistItem, error) {
	var items []ChecklistItem
	if len(data) > 256<<10 || decodeResponse(data, &items) != nil || !validChecklistItems(items) {
		return nil, mutationValidation()
	}
	return items, nil
}

func validJobProgressTime(value string) bool {
	if len(value) > 64 {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero()
}

func (c *Client) validJobUpdate(update Update, jobID string) bool {
	return validID(update.ID) && update.ProjectID == c.identity.ProjectID && update.JobID == jobID &&
		update.Sequence > 0 && validID(update.ActorID) && (update.WorkerID == "" || validID(update.WorkerID)) &&
		update.Evidence != nil && validJobProgressTime(update.CreatedAt) &&
		validateUpdateArchiveContent(updateArchiveContent{ExpectedJobRevision: update.JobRevision, Body: update.Body, Evidence: update.Evidence}) == nil
}

// JobUpdates fetches exactly one explicit newest-first archive page.
func (c *Client) JobUpdates(ctx context.Context, jobID string, options PageOptions) (Page[Update], error) {
	if options.Limit == 0 {
		options.Limit = 20
	}
	query, err := pageQuery(options)
	if err != nil || !validID(jobID) {
		return Page[Update]{}, validation()
	}
	var page Page[Update]
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", jobID, "updates"), query, true, "", nil, http.StatusOK, false, &page, 4<<20, decodeResponse); err != nil {
		return Page[Update]{}, err
	}
	if !validPage(page, options) {
		return Page[Update]{}, invalidResponse()
	}
	previous := int64(0)
	seen := make(map[string]bool, len(page.Items))
	for _, update := range page.Items {
		if !c.validJobUpdate(update, jobID) || seen[update.ID] || previous != 0 && update.Sequence >= previous {
			return Page[Update]{}, invalidResponse()
		}
		previous = update.Sequence
		seen[update.ID] = true
	}
	return page, nil
}

func (c *Client) JobChecklist(ctx context.Context, jobID string) (Checklist, error) {
	if !validID(jobID) {
		return Checklist{}, validation()
	}
	var value Checklist
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", jobID, "checklist"), nil, true, "", nil, http.StatusOK, false, &value, 128<<10, decodeResponse); err != nil {
		return Checklist{}, err
	}
	if value.JobID != jobID || value.JobRevision < 1 || value.Revision < 0 || !validChecklistItems(value.Items) || value.Revision == 0 && len(value.Items) != 0 {
		return Checklist{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) ReplaceJobChecklist(ctx context.Context, key, jobID string, request ReplaceChecklistRequest) (ChecklistReceipt, error) {
	if !validMutationKey(key) || !validID(jobID) {
		return ChecklistReceipt{}, mutationValidation()
	}
	if err := ValidateJobChecklistRequest(request); err != nil {
		return ChecklistReceipt{}, err
	}
	var result ChecklistReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("jobs", jobID, "checklist"), key, request, http.StatusOK, &result); err != nil {
		return ChecklistReceipt{}, err
	}
	if result.ID != jobID || result.JobRevision != request.ExpectedJobRevision ||
		!(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return ChecklistReceipt{}, unknownMutation("invalid_response", "The service returned an invalid checklist receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

// JobChecklistHistory fetches one revision page and never loads other history.
func (c *Client) JobChecklistHistory(ctx context.Context, jobID string, options PageOptions) (Page[ChecklistHistoryEntry], error) {
	if options.Limit == 0 {
		options.Limit = 20
	}
	query, err := pageQuery(options)
	if err != nil || !validID(jobID) {
		return Page[ChecklistHistoryEntry]{}, validation()
	}
	var page Page[ChecklistHistoryEntry]
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", jobID, "checklist", "history"), query, true, "", nil, http.StatusOK, false, &page, 4<<20, decodeResponse); err != nil {
		return Page[ChecklistHistoryEntry]{}, err
	}
	if !validPage(page, options) {
		return Page[ChecklistHistoryEntry]{}, invalidResponse()
	}
	previous := int64(0)
	for _, entry := range page.Items {
		if entry.Revision < 1 || entry.JobRevision < 1 || !validID(entry.ActorID) || entry.WorkerID != "" && !validID(entry.WorkerID) ||
			!validChecklistItems(entry.Items) || !validJobProgressTime(entry.CreatedAt) || previous != 0 && entry.Revision >= previous {
			return Page[ChecklistHistoryEntry]{}, invalidResponse()
		}
		previous = entry.Revision
	}
	return page, nil
}
