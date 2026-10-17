package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// HistorySelection selects explicit PRs, including closed and merged history.
// Selection is not a lifetime project-history quota.
type HistorySelection struct {
	PullNumbers []int64 `json:"pull_numbers"`
}
type HistoryPosition struct {
	PullIndex int    `json:"pull_index"`
	Resource  string `json:"resource"`
	Page      int64  `json:"page"`
}
type HistoryCollection struct {
	ID           string           `json:"id"`
	ProjectID    string           `json:"project_id"`
	RepositoryID int64            `json:"repository_id"`
	State        string           `json:"state"`
	Revision     int64            `json:"revision"`
	Reason       string           `json:"reason"`
	Selection    HistorySelection `json:"selection"`
	Position     HistoryPosition  `json:"position"`
	PassNumber   int64            `json:"pass_number"`
	PageCount    int64            `json:"page_count"`
	RecordCount  int64            `json:"record_count"`
	CreatedAt    string           `json:"created_at"`
	UpdatedAt    string           `json:"updated_at"`
}
type HistoryCollectionReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	State    string `json:"state"`
}

// HistoryCollectionResult encodes exactly one flat terminal operation or the
// original enqueue receipt. Waiting does not change action identity.
type HistoryCollectionResult struct {
	Receipt   *HistoryCollectionReceipt
	Operation *HistoryCollection
}

func (r HistoryCollectionResult) MarshalJSON() ([]byte, error) {
	if (r.Receipt == nil) == (r.Operation == nil) {
		return nil, strictjson.ErrSyntax
	}
	if r.Receipt != nil {
		return json.Marshal(r.Receipt)
	}
	return json.Marshal(r.Operation)
}

type HistoryPageSummary struct {
	ID         string `json:"id"`
	Sequence   int64  `json:"sequence"`
	PassNumber int64  `json:"pass_number"`
	PullNumber int64  `json:"pull_number"`
	Resource   string `json:"resource"`
	Page       int64  `json:"page"`
	Count      int64  `json:"count"`
	SHA256     string `json:"sha256"`
	CreatedAt  string `json:"created_at"`
}
type HistoryPageList struct {
	Items   []HistoryPageSummary `json:"items"`
	Cursor  string               `json:"cursor,omitempty"`
	HasMore bool                 `json:"has_more"`
}

// HistoryCapture contains untrusted source facts, never instructions or native
// review credit. Reading a capture neither imports nor applies its contents.
type HistoryCapture struct {
	Version        int              `json:"version"`
	RepositoryID   int64            `json:"repository_id"`
	RepositoryName string           `json:"repository_name"`
	PullNumber     int64            `json:"pull_number"`
	Resource       string           `json:"resource"`
	Page           int64            `json:"page"`
	CollectedAt    string           `json:"collected_at"`
	Count          int              `json:"count"`
	Facts          json.RawMessage  `json:"facts"`
	Next           *HistoryPosition `json:"next,omitempty"`
}

func ValidateHistorySelection(s HistorySelection) error {
	if len(s.PullNumbers) < 1 || len(s.PullNumbers) > 100 {
		return validation()
	}
	seen := make(map[int64]bool, len(s.PullNumbers))
	for _, n := range s.PullNumbers {
		if !validPullNumber(n) || seen[n] {
			return validation()
		}
		seen[n] = true
	}
	return nil
}
func ValidateHistoryCollectionID(id string) error {
	if !validReviewOpaqueID(id, "history_collection_") {
		return validation()
	}
	return nil
}
func ValidateHistoryPageID(id string) error {
	if !validReviewOpaqueID(id, "history_page_") {
		return validation()
	}
	return nil
}
func ValidateHistoryCollectionWait(seconds int) error {
	if seconds < 0 || seconds > 900 {
		return validation()
	}
	return nil
}
func HistoryCollectionBudget(seconds int) time.Duration {
	return time.Duration(seconds+5) * time.Second
}
func historyPositionValid(p HistoryPosition, pulls int) bool {
	return p.PullIndex >= 0 && p.PullIndex < pulls && p.Page >= 1 && p.Page <= 2147483647 && oneReviewOf(p.Resource, "pull", "reviews", "review_comments", "issue_comments") && (p.Resource != "pull" || p.Page == 1)
}
func historyTime(s string) bool {
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && !t.IsZero() && len(s) <= 40 && t.Year() >= 1 && t.Year() <= 9999
}
func (c *Client) validHistoryCollection(v HistoryCollection) bool {
	if ValidateHistoryCollectionID(v.ID) != nil || v.ProjectID != c.identity.ProjectID || v.RepositoryID < 1 || v.RepositoryID > 9007199254740991 || ValidateHistorySelection(v.Selection) != nil || !historyPositionValid(v.Position, len(v.Selection.PullNumbers)) || v.Revision < 1 || v.PassNumber < 1 || v.PassNumber > v.Revision || v.PageCount < 0 || v.RecordCount < 0 || !historyTime(v.CreatedAt) || !historyTime(v.UpdatedAt) || !oneReviewOf(v.State, "queued", "running", "paused", "completed", "canceled") {
		return false
	}
	if v.RecordCount/100 > v.PageCount || v.RecordCount/100 == v.PageCount && v.RecordCount%100 != 0 {
		return false
	}
	if v.State == "paused" {
		if !oneReviewOf(v.Reason, "configuration_changed", "authorization_changed", "rate_limited", "source_invalid", "source_changed", "source_unavailable") {
			return false
		}
	} else if v.Reason != "" {
		return false
	}
	if v.PageCount == 0 && (v.RecordCount != 0 || v.Position != (HistoryPosition{Resource: "pull", Page: 1}) || v.State == "completed") {
		return false
	}
	return v.State != "completed" || v.Position.PullIndex == len(v.Selection.PullNumbers)-1 && v.Position.Resource == "issue_comments"
}
func (c *Client) CollectHistory(ctx context.Context, key string, selection HistorySelection, waitSeconds int) (HistoryCollectionResult, error) {
	if !validMutationKey(key) || ValidateHistorySelection(selection) != nil || ValidateHistoryCollectionWait(waitSeconds) != nil {
		return HistoryCollectionResult{}, mutationValidation()
	}
	selection.PullNumbers = append([]int64(nil), selection.PullNumbers...)
	body, _ := json.Marshal(selection)
	var result HistoryCollectionResult
	var status int
	decode := func(data []byte, _ any) error {
		if status == http.StatusOK {
			result.Operation = &HistoryCollection{}
			return decodeResponse(data, result.Operation)
		}
		result.Receipt = &HistoryCollectionReceipt{}
		return decodeResponse(data, result.Receipt)
	}
	err := c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("history", "collections"), nil, true, key, body, http.StatusAccepted, true, &result, maxResponseBytes, decode, &sourceWaitOptions{seconds: waitSeconds, status: &status, budget: HistoryCollectionBudget(waitSeconds)})
	if err != nil {
		return HistoryCollectionResult{}, err
	}
	valid := false
	if v := result.Operation; v != nil {
		valid = waitSeconds > 0 && c.validHistoryCollection(*v) && slices.Equal(v.Selection.PullNumbers, selection.PullNumbers) && oneReviewOf(v.State, "completed", "paused", "canceled")
	}
	if v := result.Receipt; v != nil {
		valid = ValidateHistoryCollectionID(v.ID) == nil && v.Revision == 1 && v.State == "queued"
	}
	if !valid {
		return HistoryCollectionResult{}, unknownMutation("invalid_response", "The service returned invalid collection facts; retry with the original key and selection.", status)
	}
	return result, nil
}
func (c *Client) HistoryCollection(ctx context.Context, id string, waitSeconds int) (HistoryCollection, error) {
	if ValidateHistoryCollectionID(id) != nil || ValidateHistoryCollectionWait(waitSeconds) != nil {
		return HistoryCollection{}, validation()
	}
	var v HistoryCollection
	var status int
	if err := c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("history", "collections", id), nil, true, "", nil, http.StatusOK, false, &v, maxResponseBytes, decodeResponse, &sourceWaitOptions{seconds: waitSeconds, status: &status, budget: HistoryCollectionBudget(waitSeconds)}); err != nil {
		return HistoryCollection{}, err
	}
	if v.ID != id || !c.validHistoryCollection(v) {
		return HistoryCollection{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) ChangeHistoryCollection(ctx context.Context, id, action, key string, expectedRevision int64) (HistoryCollectionReceipt, error) {
	if ValidateHistoryCollectionID(id) != nil || !oneReviewOf(action, "resume", "cancel") || !validMutationKey(key) || expectedRevision < 1 || expectedRevision == math.MaxInt64 {
		return HistoryCollectionReceipt{}, mutationValidation()
	}
	body, _ := json.Marshal(struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}{expectedRevision})
	var v HistoryCollectionReceipt
	if err := c.request(ctx, http.MethodPost, c.projectPath("history", "collections", id, action), nil, true, key, body, http.StatusOK, true, &v); err != nil {
		return HistoryCollectionReceipt{}, err
	}
	want := "canceled"
	if action == "resume" {
		want = "queued"
	}
	if v.ID != id || v.Revision != expectedRevision+1 || v.State != want {
		return HistoryCollectionReceipt{}, unknownMutation("invalid_response", "The service returned an invalid collection action receipt; retry the original request.", 200)
	}
	return v, nil
}
func (c *Client) HistoryPages(ctx context.Context, id string, limit int, cursor string) (HistoryPageList, error) {
	if ValidateHistoryCollectionID(id) != nil || limit < 0 || limit > 100 || cursor != "" && !validReviewCursor(cursor) {
		return HistoryPageList{}, validation()
	}
	if limit == 0 {
		limit = 5
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var v HistoryPageList
	if err := c.get(ctx, c.projectPath("history", "collections", id, "pages"), query, true, &v); err != nil {
		return HistoryPageList{}, err
	}
	if v.Items == nil || len(v.Items) > limit || v.HasMore != (v.Cursor != "") || v.Cursor != "" && !validReviewCursor(v.Cursor) || v.HasMore && len(v.Items) != limit {
		return HistoryPageList{}, invalidResponse()
	}
	ids := map[string]bool{}
	var sequence, pass int64
	for _, p := range v.Items {
		if ValidateHistoryPageID(p.ID) != nil || ids[p.ID] || p.Sequence <= sequence || p.PassNumber < 1 || p.PassNumber < pass || !validPullNumber(p.PullNumber) || !historyPositionValid(HistoryPosition{Resource: p.Resource, Page: p.Page}, 1) || p.Count < 0 || p.Count > 100 || p.Resource == "pull" && p.Count != 1 || !controlHex(p.SHA256, 64) || !historyTime(p.CreatedAt) {
			return HistoryPageList{}, invalidResponse()
		}
		ids[p.ID] = true
		sequence, pass = p.Sequence, p.PassNumber
	}
	return v, nil
}
func (c *Client) HistoryCapture(ctx context.Context, id, pageID string) (HistoryCapture, error) {
	if ValidateHistoryCollectionID(id) != nil || ValidateHistoryPageID(pageID) != nil {
		return HistoryCapture{}, validation()
	}
	var v HistoryCapture
	// Facts is an opaque, strictly syntax-checked JSON value, rather than an
	// array of byte values. Validate the complete envelope separately.
	decode := func(data []byte, dst any) error {
		if err := strictjson.Decode(data, dst); err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return strictjson.ErrSyntax
		}
		for _, name := range []string{"version", "repository_id", "repository_name", "pull_number", "resource", "page", "collected_at", "count", "facts"} {
			raw, present := fields[name]
			if !present || string(raw) == "null" {
				return strictjson.ErrSyntax
			}
		}
		if raw, present := fields["next"]; present && !completeShape(raw, &HistoryPosition{}) {
			return strictjson.ErrSyntax
		}
		return nil
	}
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("history", "collections", id, "pages", pageID), nil, true, "", nil, http.StatusOK, false, &v, (8<<20)+16384, decode); err != nil {
		return HistoryCapture{}, err
	}
	if !c.validHistoryCapture(v) {
		return HistoryCapture{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) validHistoryCapture(v HistoryCapture) bool {
	parts := strings.Split(v.RepositoryName, "/")
	if v.Version != 1 || v.RepositoryID < 1 || v.RepositoryID > 9007199254740991 || len(parts) != 2 || !cleanText(parts[0], 100) || !cleanText(parts[1], 100) || !validID(parts[0]) || !validID(parts[1]) || !validPullNumber(v.PullNumber) || !historyPositionValid(HistoryPosition{Resource: v.Resource, Page: v.Page}, 1) || !historyTime(v.CollectedAt) || v.Count < 0 || v.Count > 100 || len(v.Facts) > 8<<20 {
		return false
	}
	var decoded any
	if json.Unmarshal(v.Facts, &decoded) != nil || c.hasSensitive(decoded) {
		return false
	}
	if v.Resource == "pull" {
		var facts map[string]json.RawMessage
		var number, repository int64
		if v.Count != 1 || json.Unmarshal(v.Facts, &facts) != nil || json.Unmarshal(facts["Number"], &number) != nil || json.Unmarshal(facts["RepositoryID"], &repository) != nil || number != v.PullNumber || repository != v.RepositoryID {
			return false
		}
	} else {
		var items []map[string]json.RawMessage
		if json.Unmarshal(v.Facts, &items) != nil || items == nil || len(items) != v.Count {
			return false
		}
		ids := map[int64]bool{}
		for _, item := range items {
			var id int64
			var body any
			var author map[string]json.RawMessage
			if item == nil || json.Unmarshal(item["ID"], &id) != nil || id < 1 || ids[id] || json.Unmarshal(item["Body"], &body) != nil || json.Unmarshal(item["Author"], &author) != nil || author == nil {
				return false
			}
			if _, ok := body.(string); !ok {
				return false
			}
			ids[id] = true
		}
	}
	if v.Next != nil {
		p := *v.Next
		if !historyPositionValid(p, 100) {
			return false
		}
		// Without an implicit operation read, validate adjacency independently of
		// the selection's exact pull index; the server owns that retained binding.
		if p.Resource == v.Resource {
			return v.Resource != "pull" && v.Page < 2147483647 && p.Page == v.Page+1
		}
		if p.Page != 1 {
			return false
		}
		switch v.Resource {
		case "pull":
			return p.Resource == "reviews"
		case "reviews":
			return p.Resource == "review_comments"
		case "review_comments":
			return p.Resource == "issue_comments"
		case "issue_comments":
			return p.Resource == "pull" && p.PullIndex > 0
		}
	}
	return v.Resource == "issue_comments"
}
