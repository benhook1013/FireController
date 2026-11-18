package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

var trackingKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// ValidTrackingKey reports whether v is a milestone or item key.
func ValidTrackingKey(v string) bool { return trackingKey.MatchString(v) }

// TrackingItemFilter selects list rows. Statuses is an OR set; Link is TYPE:REF.
type TrackingItemFilter struct {
	Milestone, Parent, Label, Link string
	Statuses                       []string
}

// TrackingItemInput declares an item. Nil fields take their defaults; nil
// Links keeps the item's current links.
type TrackingItemInput struct {
	Title       *string               `json:"title,omitempty"`
	Description *string               `json:"description,omitempty"`
	Status      *string               `json:"status,omitempty"`
	Milestone   *string               `json:"milestone,omitempty"`
	Parent      *string               `json:"parent,omitempty"`
	Labels      *[]string             `json:"labels,omitempty"`
	RevisitWhen *string               `json:"revisit_when,omitempty"`
	Position    *int64                `json:"position,omitempty"`
	Links       *[]trackingmodel.Link `json:"links,omitempty"`
}

// TrackingItemChanges is a partial item change. Title and description changes
// need ExpectedRevision; the others are last-write-wins without it.
type TrackingItemChanges struct {
	Title        *string   `json:"title,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Status       *string   `json:"status,omitempty"`
	Milestone    *string   `json:"milestone,omitempty"`
	Parent       *string   `json:"parent,omitempty"`
	Labels       *[]string `json:"labels,omitempty"`
	AddLabels    []string  `json:"add_labels,omitempty"`
	RemoveLabels []string  `json:"remove_labels,omitempty"`
	RevisitWhen  *string   `json:"revisit_when,omitempty"`
	Position     *int64    `json:"position,omitempty"`
}

// TrackingMilestoneInput declares or changes a milestone.
type TrackingMilestoneInput struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	State       *string `json:"state,omitempty"`
	TargetDate  *string `json:"target_date,omitempty"`
	Position    *int64  `json:"position,omitempty"`
}

// TrackingLinkChange adds and/or removes links as one idempotent set change.
type TrackingLinkChange struct {
	ExpectedRevision *int64                     `json:"expected_revision,omitempty"`
	Add              []trackingmodel.Link       `json:"add,omitempty"`
	Remove           []trackingmodel.LinkTarget `json:"remove,omitempty"`
}

func (c *Client) trackingPath(parts ...string) []string {
	return c.projectPath(append([]string{"tracking"}, parts...)...)
}

func validRevisionGuard(expected *int64, minimum int64) bool {
	return expected == nil || *expected >= minimum
}

func (c *Client) trackingWrite(ctx context.Context, method string, segments []string, key string, request any) (trackingmodel.Receipt, error) {
	if !validMutationKey(key) {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	var result trackingmodel.Receipt
	if err := c.mutate(ctx, method, segments, key, request, http.StatusOK, &result); err != nil {
		return trackingmodel.Receipt{}, err
	}
	if result.Revision < 1 || result.Outcome != "created" && result.Outcome != "updated" && result.Outcome != "unchanged" {
		return trackingmodel.Receipt{}, unknownMutation("invalid_response", "The service returned an invalid tracking receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

// PutTrackingItem declares one item by key. expected nil skips the guard and 0
// requires that the item does not exist yet.
func (c *Client) PutTrackingItem(ctx context.Context, key, itemKey string, expected *int64, item TrackingItemInput) (trackingmodel.Receipt, error) {
	if !ValidTrackingKey(itemKey) || item.Title == nil || !validRevisionGuard(expected, 0) {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	return c.trackingWrite(ctx, http.MethodPut, c.trackingPath("items", itemKey), key, struct {
		ExpectedRevision *int64            `json:"expected_revision,omitempty"`
		Item             TrackingItemInput `json:"item"`
	}{expected, item})
}

func (c *Client) PatchTrackingItem(ctx context.Context, key, itemKey string, expected *int64, changes TrackingItemChanges) (trackingmodel.Receipt, error) {
	if !ValidTrackingKey(itemKey) || !validRevisionGuard(expected, 1) || (changes.Title != nil || changes.Description != nil) && expected == nil {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	return c.trackingWrite(ctx, http.MethodPatch, c.trackingPath("items", itemKey), key, struct {
		ExpectedRevision *int64              `json:"expected_revision,omitempty"`
		Changes          TrackingItemChanges `json:"changes"`
	}{expected, changes})
}

func (c *Client) ChangeTrackingLinks(ctx context.Context, key, itemKey string, change TrackingLinkChange) (trackingmodel.Receipt, error) {
	if !ValidTrackingKey(itemKey) || len(change.Add)+len(change.Remove) == 0 || !validRevisionGuard(change.ExpectedRevision, 1) {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	return c.trackingWrite(ctx, http.MethodPost, c.trackingPath("items", itemKey, "links"), key, change)
}

func (c *Client) PutTrackingMilestone(ctx context.Context, key, milestoneKey string, expected *int64, milestone TrackingMilestoneInput) (trackingmodel.Receipt, error) {
	if !ValidTrackingKey(milestoneKey) || milestone.Title == nil || !validRevisionGuard(expected, 0) {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	return c.trackingWrite(ctx, http.MethodPut, c.trackingPath("milestones", milestoneKey), key, struct {
		ExpectedRevision *int64                 `json:"expected_revision,omitempty"`
		Milestone        TrackingMilestoneInput `json:"milestone"`
	}{expected, milestone})
}

func (c *Client) PatchTrackingMilestone(ctx context.Context, key, milestoneKey string, expected *int64, changes TrackingMilestoneInput) (trackingmodel.Receipt, error) {
	if !ValidTrackingKey(milestoneKey) || !validRevisionGuard(expected, 1) || (changes.Title != nil || changes.Description != nil) && expected == nil {
		return trackingmodel.Receipt{}, mutationValidation()
	}
	return c.trackingWrite(ctx, http.MethodPatch, c.trackingPath("milestones", milestoneKey), key, struct {
		ExpectedRevision *int64                 `json:"expected_revision,omitempty"`
		Changes          TrackingMilestoneInput `json:"changes"`
	}{expected, changes})
}

// DecodeTrackingBatch strictly decodes a batch declaration file before any
// connection is made. The server applies the full validation.
func DecodeTrackingBatch(data []byte) (json.RawMessage, error) {
	var shape struct {
		Milestones []json.RawMessage `json:"milestones"`
		Items      []json.RawMessage `json:"items"`
	}
	if len(data) > 2<<20 || strictjson.Decode(data, &shape) != nil || len(shape.Items)+len(shape.Milestones) == 0 || len(shape.Items) > 100 || len(shape.Milestones) > 20 {
		return nil, mutationValidation()
	}
	return json.RawMessage(data), nil
}

func (c *Client) TrackingBatch(ctx context.Context, key string, document json.RawMessage) (trackingmodel.BatchReceipt, error) {
	if !validMutationKey(key) {
		return trackingmodel.BatchReceipt{}, mutationValidation()
	}
	if _, err := DecodeTrackingBatch(document); err != nil {
		return trackingmodel.BatchReceipt{}, err
	}
	var result trackingmodel.BatchReceipt
	if err := c.request(ctx, http.MethodPost, c.trackingPath("batch"), nil, true, key, document, http.StatusOK, true, &result); err != nil {
		return trackingmodel.BatchReceipt{}, err
	}
	return result, nil
}

func (c *Client) TrackingItems(ctx context.Context, options PageOptions, filter TrackingItemFilter) (trackingmodel.Page[trackingmodel.ItemSummary], error) {
	var out trackingmodel.Page[trackingmodel.ItemSummary]
	options, q, err := projectMapPage(options)
	if err != nil {
		return out, err
	}
	for name, v := range map[string]string{"milestone": filter.Milestone, "parent": filter.Parent} {
		if v != "" {
			if !ValidTrackingKey(v) {
				return out, validation()
			}
			q.Set(name, v)
		}
	}
	if filter.Label != "" {
		q.Set("label", filter.Label)
	}
	if filter.Link != "" {
		q.Set("link", filter.Link)
	}
	if len(filter.Statuses) > 0 {
		for _, s := range filter.Statuses {
			if !slices.Contains(trackingmodel.Statuses, s) {
				return out, validation()
			}
		}
		q.Set("status", strings.Join(filter.Statuses, ","))
	}
	if err = c.get(ctx, c.trackingPath("items"), q, true, &out); err != nil {
		return out, err
	}
	if len(out.Items) > options.Limit || out.HasMore != (out.Cursor != "") {
		return trackingmodel.Page[trackingmodel.ItemSummary]{}, invalidResponse()
	}
	return out, nil
}

func (c *Client) TrackingItem(ctx context.Context, itemKey string) (trackingmodel.ItemView, error) {
	var out trackingmodel.ItemView
	if !ValidTrackingKey(itemKey) {
		return out, validation()
	}
	if err := c.get(ctx, c.trackingPath("items", itemKey), nil, true, &out); err != nil {
		return out, err
	}
	if out.Item.Key != itemKey {
		return trackingmodel.ItemView{}, invalidResponse()
	}
	return out, nil
}

func (c *Client) TrackingMilestones(ctx context.Context, state string) (trackingmodel.Page[trackingmodel.MilestoneSummary], error) {
	var out trackingmodel.Page[trackingmodel.MilestoneSummary]
	q := url.Values{}
	if state != "" {
		if state != "open" && state != "closed" {
			return out, validation()
		}
		q.Set("state", state)
	}
	err := c.get(ctx, c.trackingPath("milestones"), q, true, &out)
	return out, err
}

func (c *Client) TrackingMilestone(ctx context.Context, milestoneKey string) (trackingmodel.MilestoneView, error) {
	var out trackingmodel.MilestoneView
	if !ValidTrackingKey(milestoneKey) {
		return out, validation()
	}
	if err := c.get(ctx, c.trackingPath("milestones", milestoneKey), nil, true, &out); err != nil {
		return out, err
	}
	if out.Milestone.Key != milestoneKey {
		return trackingmodel.MilestoneView{}, invalidResponse()
	}
	return out, nil
}

// TrackingHistory reads one page of an item's or milestone's revisions. kind is
// "items" or "milestones".
func (c *Client) TrackingHistory(ctx context.Context, kind, key string, options PageOptions) (trackingmodel.Page[trackingmodel.HistorySummary], error) {
	var out trackingmodel.Page[trackingmodel.HistorySummary]
	if kind != "items" && kind != "milestones" || !ValidTrackingKey(key) {
		return out, validation()
	}
	_, q, err := projectMapPage(options)
	if err != nil {
		return out, err
	}
	err = c.get(ctx, c.trackingPath(kind, key, "history"), q, true, &out)
	return out, err
}

func (c *Client) TrackingItemRevision(ctx context.Context, key string, revision int64) (trackingmodel.ItemRevision, error) {
	var out trackingmodel.ItemRevision
	if !ValidTrackingKey(key) || revision < 1 {
		return out, validation()
	}
	err := c.get(ctx, c.trackingPath("items", key, "history", strconv.FormatInt(revision, 10)), nil, true, &out)
	return out, err
}

func (c *Client) TrackingMilestoneRevision(ctx context.Context, key string, revision int64) (trackingmodel.MilestoneRevision, error) {
	var out trackingmodel.MilestoneRevision
	if !ValidTrackingKey(key) || revision < 1 {
		return out, validation()
	}
	err := c.get(ctx, c.trackingPath("milestones", key, "history", strconv.FormatInt(revision, 10)), nil, true, &out)
	return out, err
}
