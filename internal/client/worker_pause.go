package client

import (
	"context"
	"math"
	"net/http"
	"unicode"
	"unicode/utf8"
)

// WorkerPause is explicit coordination state, not presence or execution control.
type WorkerPause struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	Paused   bool   `json:"paused"`
	Reason   string `json:"reason"`
}

type ReplaceWorkerPauseRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Paused           bool   `json:"paused"`
	Reason           string `json:"reason"`
}

type WorkerPauseReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type WorkerPauseHistoryEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	Paused        bool   `json:"paused"`
	Reason        string `json:"reason"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	CreatedAt     string `json:"created_at"`
}

func validWorkerPause(paused bool, reason string) bool {
	if !utf8.ValidString(reason) || len(reason) > 1024 || !paused && reason != "" {
		return false
	}
	for _, r := range reason {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return false
		}
	}
	return true
}

// ValidateWorkerPauseRequest is local only and preserves exact reason text.
func ValidateWorkerPauseRequest(request ReplaceWorkerPauseRequest) error {
	if request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected worker-pause revision.", "Read the bound worker's pause state and supply its independent revision explicitly.", 400)
	}
	if !validWorkerPause(request.Paused, request.Reason) {
		return mutationValidation()
	}
	return nil
}

func (c *Client) workerPausePath(worker string, parts ...string) []string {
	return c.projectPath(append([]string{"workers", worker, "pause"}, parts...)...)
}

// WorkerPause reads only current pause state, never jobs, focus or history.
func (c *Client) WorkerPause(ctx context.Context, selectedWorker ...string) (WorkerPause, error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return WorkerPause{}, err
	}
	var value WorkerPause
	// A 1024-byte reason may expand sixfold in canonical escaped JSON.
	if err := c.requestBounded(ctx, http.MethodGet, c.workerPausePath(worker), nil, true, "", nil, http.StatusOK, false, &value, 8<<10, decodeResponse); err != nil {
		return WorkerPause{}, err
	}
	if value.WorkerID != worker || value.Revision < 0 || !validWorkerPause(value.Paused, value.Reason) || value.Revision == 0 && value.Paused {
		return WorkerPause{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) ReplaceWorkerPause(ctx context.Context, key string, request ReplaceWorkerPauseRequest, selectedWorker ...string) (WorkerPauseReceipt, error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return WorkerPauseReceipt{}, err
	}
	if !validMutationKey(key) {
		return WorkerPauseReceipt{}, mutationValidation()
	}
	if err := ValidateWorkerPauseRequest(request); err != nil {
		return WorkerPauseReceipt{}, err
	}
	var value WorkerPauseReceipt
	if err := c.mutate(ctx, http.MethodPut, c.workerPausePath(worker), key, request, http.StatusOK, &value); err != nil {
		return WorkerPauseReceipt{}, err
	}
	if value.ID != worker || !(value.Outcome == "updated" && value.Revision == request.ExpectedRevision+1 || value.Outcome == "unchanged" && value.Revision == request.ExpectedRevision) {
		return WorkerPauseReceipt{}, unknownMutation("invalid_response", "The service returned an invalid worker-pause receipt; its outcome is unknown.", http.StatusOK)
	}
	return value, nil
}

// WorkerPauseHistory reads one explicit revision page without automatic followup.
func (c *Client) WorkerPauseHistory(ctx context.Context, options PageOptions, selectedWorker ...string) (Page[WorkerPauseHistoryEntry], error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return Page[WorkerPauseHistoryEntry]{}, err
	}
	if options.Limit == 0 {
		options.Limit = 20
	}
	query, err := pageQuery(options)
	if err != nil || options.Cursor != "" && !validOpaqueCursor(options.Cursor) {
		return Page[WorkerPauseHistoryEntry]{}, validation()
	}
	var page Page[WorkerPauseHistoryEntry]
	if err = c.get(ctx, c.workerPausePath(worker, "history"), query, true, &page); err != nil {
		return Page[WorkerPauseHistoryEntry]{}, err
	}
	if !validPage(page, options) || page.Cursor != "" && !validOpaqueCursor(page.Cursor) {
		return Page[WorkerPauseHistoryEntry]{}, invalidResponse()
	}
	var previous int64
	for _, entry := range page.Items {
		if entry.WorkerID != worker || entry.Revision < 1 || !validWorkerPause(entry.Paused, entry.Reason) || !validID(entry.ActorID) || entry.ActorWorkerID != "" && !validID(entry.ActorWorkerID) || !validJobProgressTime(entry.CreatedAt) || previous != 0 && entry.Revision >= previous {
			return Page[WorkerPauseHistoryEntry]{}, invalidResponse()
		}
		previous = entry.Revision
	}
	return page, nil
}
