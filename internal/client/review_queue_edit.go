package client

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ReviewQueueEditReceipt identifies the immutable queue edit snapshot.
type ReviewQueueEditReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type reviewQueueEditDocument struct {
	ExpectedRevision *int64                      `json:"expected_revision"`
	RepositoryID     int64                       `json:"repository_id"`
	Source           string                      `json:"source"`
	Queue            []reviewQueueEditPull       `json:"queue"`
	Retired          []reviewQueueEditRetirement `json:"retired"`
}
type reviewQueueEditPull struct {
	Diff         *ReviewPullDiff `json:"diff,omitempty"`
	IsDraft      *bool           `json:"is_draft,omitempty"`
	MergedAt     string          `json:"merged_at,omitempty"`
	Number       int64           `json:"number"`
	Title        string          `json:"title"`
	State        string          `json:"state"`
	HeadSHA      string          `json:"head_sha"`
	BaseRef      string          `json:"base_ref"`
	BaseSHA      string          `json:"base_sha"`
	CIState      string          `json:"ci_state"`
	Mergeability string          `json:"mergeability"`
}
type reviewQueueEditRetirement struct {
	Number int64  `json:"number"`
	Reason string `json:"reason"`
}

func queueEditValidation() error {
	return fail("validation_failed", "Provide one bounded strict queue edit JSON document with 1 to 100 distinct selected PRs.", "Correct the selected metadata or retirements and retain the explicit key and destination binding.", http.StatusBadRequest)
}

// ValidateReviewQueueEditDocument validates metadata-only edits before loading
// credentials. Retained-history, current revision and administrator checks belong
// to the service. Omitted arrays and optional SHA/base fields normalize there.
func ValidateReviewQueueEditDocument(document json.RawMessage) error {
	_, err := reviewQueueEditDocumentValue(document)
	return err
}

// ValidateReviewQueueEdit validates the explicit key and document before I/O.
func ValidateReviewQueueEdit(key string, document json.RawMessage) error {
	if !validMutationKey(key) {
		return mutationValidation()
	}
	return ValidateReviewQueueEditDocument(document)
}

func reviewQueueEditDocumentValue(document json.RawMessage) (reviewQueueEditDocument, error) {
	var value reviewQueueEditDocument
	if len(document) == 0 || len(document) > 256<<10 || strictjson.Decode(document, &value) != nil || !queueEditNonNullObject(document) {
		return value, queueEditValidation()
	}
	if value.ExpectedRevision == nil || *value.ExpectedRevision < 0 || value.RepositoryID <= 0 || !cleanReviewText(value.Source, 128, true) {
		return value, queueEditValidation()
	}
	count := len(value.Queue) + len(value.Retired)
	if count < 1 || count > 100 {
		return value, queueEditValidation()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(document, &fields)
	for _, name := range []string{"queue", "retired"} {
		if raw, ok := fields[name]; ok {
			var items []json.RawMessage
			_ = json.Unmarshal(raw, &items)
			for _, item := range items {
				if !queueEditNonNullObject(item) {
					return value, queueEditValidation()
				}
			}
		}
	}
	seen := make(map[int64]bool, count)
	for _, pull := range value.Queue {
		if pull.Number < 1 || pull.Number > 2147483647 || seen[pull.Number] || !cleanReviewText(pull.Title, 500, true) || !oneReviewOf(pull.State, "open", "merged", "closed", "unknown") || !validReviewSHA(pull.HeadSHA) || !cleanReviewText(pull.BaseRef, 256, false) || !validReviewSHA(pull.BaseSHA) || !oneReviewOf(pull.CIState, "unknown", "pending", "success", "failure") || !oneReviewOf(pull.Mergeability, "unknown", "mergeable", "conflicting") {
			return value, queueEditValidation()
		}
		if !validReviewPullMetadata(ReviewPullSummary{State: pull.State, Diff: pull.Diff, IsDraft: pull.IsDraft, MergedAt: pull.MergedAt}) {
			return value, queueEditValidation()
		}
		seen[pull.Number] = true
	}
	for _, retired := range value.Retired {
		if retired.Number < 1 || retired.Number > 2147483647 || seen[retired.Number] || !oneReviewOf(retired.Reason, "superseded", "defunct") {
			return value, queueEditValidation()
		}
		seen[retired.Number] = true
	}
	if value.Queue == nil {
		value.Queue = []reviewQueueEditPull{}
	}
	if value.Retired == nil {
		value.Retired = []reviewQueueEditRetirement{}
	}
	normalized, err := json.Marshal(value)
	if err != nil || len(normalized) > 256<<10 {
		return value, queueEditValidation()
	}
	return value, nil
}
func queueEditNonNullObject(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

// EditReviewQueue makes one bounded administrator mutation. It never imports
// history, retries a dispatched request or generates an idempotency key.
func (c *Client) EditReviewQueue(ctx context.Context, key string, document json.RawMessage) (ReviewQueueEditReceipt, error) {
	if !validMutationKey(key) {
		return ReviewQueueEditReceipt{}, mutationValidation()
	}
	expected, err := reviewQueueEditDocumentValue(document)
	if err != nil {
		return ReviewQueueEditReceipt{}, err
	}
	var receipt ReviewQueueEditReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "queue", "edits"), nil, true, key, document, http.StatusCreated, true, &receipt, 4<<10, decodeReviewResponse); err != nil {
		return ReviewQueueEditReceipt{}, err
	}
	if !validReviewSnapshot(receipt.ID) || receipt.Revision < 1 || *expected.ExpectedRevision == math.MaxInt64 || receipt.Revision != *expected.ExpectedRevision+1 || receipt.Outcome != "queue_edited" {
		return ReviewQueueEditReceipt{}, unknownMutation("invalid_response", "The service returned an invalid queue edit receipt; its outcome is unknown.", http.StatusCreated)
	}
	return receipt, nil
}
