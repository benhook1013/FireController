package client

import (
	"context"
	"math"
	"net/http"
)

// PrimaryJob is the bound worker's explicit focus, independent of job revisions.
type PrimaryJob struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	JobID    string `json:"job_id"`
}

type ReplacePrimaryJobRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	JobID            string `json:"job_id"`
}

type PrimaryJobReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type PrimaryJobHistoryEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	JobID         string `json:"job_id"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	Reason        string `json:"reason"`
	CreatedAt     string `json:"created_at"`
}

// ValidatePrimaryJobRequest performs local validation before any credential or
// service access. Empty JobID is an explicit clear, not an omitted selector.
func ValidatePrimaryJobRequest(request ReplacePrimaryJobRequest) error {
	if request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected primary-job revision.", "Read the bound worker's primary job and supply its focus revision explicitly.", 400)
	}
	if request.JobID != "" && !validID(request.JobID) {
		return mutationValidation()
	}
	return nil
}

func (c *Client) primaryJobPath(worker string, parts ...string) []string {
	return c.projectPath(append([]string{"workers", worker, "primary-job"}, parts...)...)
}

// PrimaryJob reads only focus; it never retrieves the selected job or its history.
func (c *Client) PrimaryJob(ctx context.Context, selectedWorker ...string) (PrimaryJob, error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return PrimaryJob{}, err
	}
	var value PrimaryJob
	if err := c.requestBounded(ctx, http.MethodGet, c.primaryJobPath(worker), nil, true, "", nil, http.StatusOK, false, &value, 4<<10, decodeResponse); err != nil {
		return PrimaryJob{}, err
	}
	if value.WorkerID != worker || value.Revision < 0 || value.JobID != "" && !validID(value.JobID) || value.Revision == 0 && value.JobID != "" {
		return PrimaryJob{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) ReplacePrimaryJob(ctx context.Context, key string, request ReplacePrimaryJobRequest, selectedWorker ...string) (PrimaryJobReceipt, error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return PrimaryJobReceipt{}, err
	}
	if !validMutationKey(key) {
		return PrimaryJobReceipt{}, mutationValidation()
	}
	if err := ValidatePrimaryJobRequest(request); err != nil {
		return PrimaryJobReceipt{}, err
	}
	var value PrimaryJobReceipt
	if err := c.mutate(ctx, http.MethodPut, c.primaryJobPath(worker), key, request, http.StatusOK, &value); err != nil {
		return PrimaryJobReceipt{}, err
	}
	if value.ID != worker || !(value.Outcome == "updated" && value.Revision == request.ExpectedRevision+1 || value.Outcome == "unchanged" && value.Revision == request.ExpectedRevision) {
		return PrimaryJobReceipt{}, unknownMutation("invalid_response", "The service returned an invalid primary-job receipt; its outcome is unknown.", http.StatusOK)
	}
	return value, nil
}

// PrimaryJobHistory returns exactly one explicit newest-first focus page.
func (c *Client) PrimaryJobHistory(ctx context.Context, options PageOptions, selectedWorker ...string) (Page[PrimaryJobHistoryEntry], error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return Page[PrimaryJobHistoryEntry]{}, err
	}
	if options.Limit == 0 {
		options.Limit = 20
	}
	query, err := pageQuery(options)
	if options.Cursor != "" && !validOpaqueCursor(options.Cursor) {
		return Page[PrimaryJobHistoryEntry]{}, validation()
	}
	if err != nil {
		return Page[PrimaryJobHistoryEntry]{}, err
	}
	var page Page[PrimaryJobHistoryEntry]
	if err = c.get(ctx, c.primaryJobPath(worker, "history"), query, true, &page); err != nil {
		return Page[PrimaryJobHistoryEntry]{}, err
	}
	if !validPage(page, options) || page.Cursor != "" && !validOpaqueCursor(page.Cursor) {
		return Page[PrimaryJobHistoryEntry]{}, invalidResponse()
	}
	var previous int64
	for _, entry := range page.Items {
		if entry.WorkerID != worker || entry.Revision < 1 || !validID(entry.ActorID) || entry.ActorWorkerID != "" && !validID(entry.ActorWorkerID) || !validJobProgressTime(entry.CreatedAt) || previous != 0 && entry.Revision >= previous {
			return Page[PrimaryJobHistoryEntry]{}, invalidResponse()
		}
		switch entry.Reason {
		case "selected":
			if !validID(entry.JobID) {
				return Page[PrimaryJobHistoryEntry]{}, invalidResponse()
			}
		case "cleared", "job_reassigned":
			if entry.JobID != "" {
				return Page[PrimaryJobHistoryEntry]{}, invalidResponse()
			}
		default:
			return Page[PrimaryJobHistoryEntry]{}, invalidResponse()
		}
		previous = entry.Revision
	}
	return page, nil
}
