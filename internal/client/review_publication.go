package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ReviewCheckpointRequest struct {
	EvidenceID       string `json:"evidence_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}
type ReviewCheckpointRecovery struct {
	ExpectedRevision int64 `json:"expected_revision"`
	RetryUnposted    bool  `json:"retry_unposted,omitempty"`
}
type ReviewCheckpointPublication struct {
	ID                 string       `json:"id"`
	ProjectID          string       `json:"project_id"`
	EvidenceID         string       `json:"evidence_id"`
	State              string       `json:"state"`
	Revision           int64        `json:"revision"`
	Reason             string       `json:"reason"`
	SourceRevision     int64        `json:"source_revision"`
	Anchor             ReviewAnchor `json:"anchor"`
	Counts             ReviewCounts `json:"counts"`
	CreatedAt          string       `json:"created_at"`
	UpdatedAt          string       `json:"updated_at"`
	CommentID          int64        `json:"comment_id"`
	CommentURL         string       `json:"comment_url"`
	PublishedAt        string       `json:"published_at"`
	ProviderAuthorID   int64        `json:"provider_author_id"`
	ProviderAuthorKind string       `json:"provider_author_kind"`
}

// ReviewCheckpointResult keeps terminal facts and fallback receipts flat.
type ReviewCheckpointResult struct {
	Receipt   *ReviewRequestReceipt
	Operation *ReviewCheckpointPublication
}

func (r ReviewCheckpointResult) MarshalJSON() ([]byte, error) {
	if (r.Receipt == nil) == (r.Operation == nil) {
		return nil, errors.New("invalid checkpoint publication result")
	}
	if r.Operation != nil {
		return json.Marshal(r.Operation)
	}
	return json.Marshal(r.Receipt)
}
func ValidateReviewCheckpointID(id string) error {
	if !validReviewOpaqueID(id, "review_publication_") {
		return validation()
	}
	return nil
}
func ValidateReviewCheckpointRequest(r ReviewCheckpointRequest) error {
	if ValidateReviewEvidenceID(r.EvidenceID) != nil || r.ExpectedRevision < 0 {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewCheckpointRecovery(r ReviewCheckpointRecovery) error {
	if r.ExpectedRevision < 1 || r.ExpectedRevision == math.MaxInt64 {
		return mutationValidation()
	}
	return nil
}

func (c *Client) validCheckpointPublication(v ReviewCheckpointPublication) bool {
	if ValidateReviewCheckpointID(v.ID) != nil || v.ProjectID != c.identity.ProjectID || ValidateReviewEvidenceID(v.EvidenceID) != nil || !oneReviewOf(v.State, "queued", "preparing", "posting", "recover_queued", "recovering", "published", "held", "uncertain", "cancelled") || v.Revision < 1 || v.SourceRevision < 1 || v.Reason != "" && !validExecutionIdentifier(v.Reason) || !validControlAnchor(v.Anchor) || v.Anchor.ParentPullNumber == v.Anchor.PullNumber || v.Counts.Found < 0 || v.Counts.Found > 100 || v.Counts.Accepted < 0 || v.Counts.Routed < 0 || v.Counts.Accepted > v.Counts.Found || v.Counts.Routed > v.Counts.Found-v.Counts.Accepted || !controlTime(v.CreatedAt) || !controlTime(v.UpdatedAt) || v.CommentID < 0 || v.ProviderAuthorID < 0 {
		return false
	}
	created, _ := time.Parse(time.RFC3339Nano, v.CreatedAt)
	updated, _ := time.Parse(time.RFC3339Nano, v.UpdatedAt)
	if updated.Before(created) {
		return false
	}
	if v.State != "published" {
		return v.CommentID == 0 && v.CommentURL == "" && v.PublishedAt == "" && v.ProviderAuthorID == 0 && v.ProviderAuthorKind == ""
	}
	if v.Reason != "" || v.CommentID < 1 || v.ProviderAuthorID < 1 || !oneReviewOf(v.ProviderAuthorKind, "User", "Bot", "Organization") || !controlTime(v.PublishedAt) || !strings.HasSuffix(v.PublishedAt, "Z") {
		return false
	}
	u, err := url.Parse(v.CommentURL)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.RawPath == "" && u.Fragment == "issuecomment-"+strconv.FormatInt(v.CommentID, 10) && validCheckpointCommentPath(u.Path, v.Anchor)
}
func validCheckpointCommentPath(path string, anchor ReviewAnchor) bool {
	// A stable repository may have been renamed after the captured anchor.
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[3] != strconv.FormatInt(anchor.PullNumber, 10) {
		return false
	}
	anchor.Repository = parts[0] + "/" + parts[1]
	return validControlAnchor(anchor)
}

func (c *Client) RequestReviewCheckpoint(ctx context.Context, key string, request ReviewCheckpointRequest, waitSeconds int) (ReviewCheckpointResult, error) {
	if !validMutationKey(key) || ValidateReviewSourceWait(waitSeconds) != nil || ValidateReviewCheckpointRequest(request) != nil {
		return ReviewCheckpointResult{}, mutationValidation()
	}
	return c.checkpointMutation(ctx, key, c.projectPath("reviews", "publications"), request, waitSeconds, "", request.EvidenceID)
}
func (c *Client) RecoverReviewCheckpoint(ctx context.Context, key, id string, request ReviewCheckpointRecovery, waitSeconds int) (ReviewCheckpointResult, error) {
	if !validMutationKey(key) || ValidateReviewSourceWait(waitSeconds) != nil || ValidateReviewCheckpointID(id) != nil || ValidateReviewCheckpointRecovery(request) != nil {
		return ReviewCheckpointResult{}, mutationValidation()
	}
	return c.checkpointMutation(ctx, key, c.projectPath("reviews", "publications", id, "recovery"), request, waitSeconds, id, "")
}
func (c *Client) checkpointMutation(ctx context.Context, key string, path []string, request any, seconds int, id, evidence string) (ReviewCheckpointResult, error) {
	if !validMutationKey(key) || ValidateReviewSourceWait(seconds) != nil {
		return ReviewCheckpointResult{}, mutationValidation()
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ReviewCheckpointResult{}, mutationValidation()
	}
	var value ReviewCheckpointResult
	var status int
	decode := func(data []byte, _ any) error {
		if status == http.StatusOK {
			value.Operation = &ReviewCheckpointPublication{}
			return decodeReviewResponse(data, value.Operation)
		}
		value.Receipt = &ReviewRequestReceipt{}
		return decodeReviewResponse(data, value.Receipt)
	}
	if err = c.requestBoundedOptions(ctx, http.MethodPost, path, nil, true, key, body, http.StatusAccepted, true, &value, 16<<10, decode, &sourceWaitOptions{seconds: seconds, status: &status}); err != nil {
		return ReviewCheckpointResult{}, err
	}
	valid := false
	if v := value.Operation; v != nil {
		valid = c.validCheckpointPublication(*v) && oneReviewOf(v.State, "published", "held", "uncertain", "cancelled") && (id == "" || v.ID == id) && (evidence == "" || v.EvidenceID == evidence)
	} else if v := value.Receipt; v != nil {
		valid = ValidateReviewCheckpointID(v.ID) == nil && v.State == "queued" && v.Revision >= 1 && (id == "" || v.ID == id)
	}
	if !valid {
		return ReviewCheckpointResult{}, unknownMutation("invalid_response", "The service returned invalid checkpoint publication facts; its outcome is unknown.", status)
	}
	return value, nil
}
func (c *Client) ReviewCheckpointPublication(ctx context.Context, id string) (ReviewCheckpointPublication, error) {
	if ValidateReviewCheckpointID(id) != nil {
		return ReviewCheckpointPublication{}, validation()
	}
	var value ReviewCheckpointPublication
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "publications", id), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse); err != nil {
		return ReviewCheckpointPublication{}, err
	}
	if value.ID != id || !c.validCheckpointPublication(value) {
		return ReviewCheckpointPublication{}, invalidResponse()
	}
	return value, nil
}
