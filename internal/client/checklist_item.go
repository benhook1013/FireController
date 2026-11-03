package client

import (
	"context"
	"math"
	"net/http"
)

type UpdateChecklistItemRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	Done             bool  `json:"done"`
}

func ValidChecklistItemID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, ch := range []byte(id) {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func ValidateChecklistItemRequest(request UpdateChecklistItemRequest) error {
	if request.ExpectedRevision < 1 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable current checklist revision.", "Read the current checklist and supply its revision explicitly.", 400)
	}
	return nil
}

// UpdateJobChecklistItem changes only an existing item's completion state. Its
// checklist guard is independent of unrelated job metadata or progress edits.
func (c *Client) UpdateJobChecklistItem(ctx context.Context, key, jobID, itemID string, request UpdateChecklistItemRequest) (ChecklistReceipt, error) {
	if !validMutationKey(key) || !validID(jobID) || !ValidChecklistItemID(itemID) {
		return ChecklistReceipt{}, mutationValidation()
	}
	if err := ValidateChecklistItemRequest(request); err != nil {
		return ChecklistReceipt{}, err
	}
	var result ChecklistReceipt
	if err := c.mutate(ctx, http.MethodPatch, c.projectPath("jobs", jobID, "checklist", "items", itemID), key, request, http.StatusOK, &result); err != nil {
		return ChecklistReceipt{}, err
	}
	if result.ID != jobID || result.JobRevision < 1 || !(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return ChecklistReceipt{}, unknownMutation("invalid_response", "The service returned an invalid checklist receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
