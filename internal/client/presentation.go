package client

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/presentationmodel"
)

type PresentationConfig = presentationmodel.Config

type Presentation struct {
	ProjectID    string                          `json:"project_id"`
	Revision     int64                           `json:"revision"`
	Name         string                          `json:"name"`
	BrandName    string                          `json:"brand_name"`
	Tagline      string                          `json:"tagline"`
	Headings     map[string]string               `json:"headings"`
	Theme        map[string]string               `json:"theme"`
	ReviewGroups []presentationmodel.ReviewGroup `json:"review_groups,omitempty"`
	UpdatedAt    string                          `json:"updated_at"`
}

func (p Presentation) Config() PresentationConfig {
	return PresentationConfig{Name: p.Name, BrandName: p.BrandName, Tagline: p.Tagline, Headings: p.Headings, Theme: p.Theme, ReviewGroups: p.ReviewGroups}
}

type PresentationSnapshot struct {
	Presentation Presentation `json:"presentation"`
	EventCursor  string       `json:"event_cursor"`
}
type PresentationHistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}
type PresentationRevision struct {
	Revision  int64        `json:"revision"`
	ActorID   string       `json:"actor_id"`
	CreatedAt string       `json:"created_at"`
	State     Presentation `json:"state"`
}
type ReplacePresentationRequest struct {
	ExpectedRevision int64              `json:"expected_revision"`
	Presentation     PresentationConfig `json:"presentation"`
}
type PresentationReceipt struct {
	ProjectID string `json:"project_id"`
	Revision  int64  `json:"revision"`
	Outcome   string `json:"outcome"`
}

func DecodePresentationConfig(raw []byte) (PresentationConfig, error) {
	c, err := presentationmodel.Decode(raw)
	if err != nil {
		return PresentationConfig{}, mutationValidation()
	}
	return c, nil
}

func ValidateReplacePresentationRequest(r ReplacePresentationRequest) error {
	if r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 {
		return mutationValidation()
	}
	_, err := presentationmodel.Normalize(r.Presentation)
	if err != nil {
		return mutationValidation()
	}
	return nil
}

func (c *Client) validPresentation(p Presentation) bool {
	if p.ProjectID != c.identity.ProjectID || p.Revision < 0 || p.Headings == nil || p.Theme == nil || p.Revision == 0 && p.UpdatedAt != "" || p.Revision > 0 && !validJobProgressTime(p.UpdatedAt) {
		return false
	}
	if p.Revision == 0 {
		defaults := presentationmodel.Defaults()
		if p.BrandName != defaults.BrandName || p.Tagline != defaults.Tagline || len(p.Headings) != 0 || len(p.Theme) != 0 || len(p.ReviewGroups) != 0 {
			return false
		}
		return p.Name != "" && len(p.Name) <= 200 && utf8.ValidString(p.Name)
	}
	normalized, err := presentationmodel.Normalize(p.Config())
	return err == nil && reflect.DeepEqual(normalized, p.Config())
}

func (c *Client) Presentation(ctx context.Context) (PresentationSnapshot, error) {
	var result PresentationSnapshot
	if err := c.get(ctx, c.projectPath("presentation"), nil, true, &result); err != nil {
		return PresentationSnapshot{}, err
	}
	if !c.validPresentation(result.Presentation) || !validOpaqueCursor(result.EventCursor) {
		return PresentationSnapshot{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) PresentationHistory(ctx context.Context, options PageOptions) (Page[PresentationHistorySummary], error) {
	options, q, err := projectMapPage(options)
	if err != nil {
		return Page[PresentationHistorySummary]{}, err
	}
	var result Page[PresentationHistorySummary]
	if err = c.get(ctx, c.projectPath("presentation", "history"), q, true, &result); err != nil {
		return Page[PresentationHistorySummary]{}, err
	}
	if !validPage(result, options) || result.Cursor != "" && !validOpaqueCursor(result.Cursor) {
		return Page[PresentationHistorySummary]{}, invalidResponse()
	}
	var previous int64
	for _, item := range result.Items {
		if item.Revision < 1 || !validID(item.ActorID) || !validJobProgressTime(item.CreatedAt) || previous != 0 && item.Revision >= previous {
			return Page[PresentationHistorySummary]{}, invalidResponse()
		}
		previous = item.Revision
	}
	return result, nil
}

func (c *Client) PresentationRevision(ctx context.Context, revision int64) (PresentationRevision, error) {
	if revision < 1 {
		return PresentationRevision{}, validation()
	}
	var result PresentationRevision
	if err := c.get(ctx, c.projectPath("presentation", "history", strconv.FormatInt(revision, 10)), nil, true, &result); err != nil {
		return PresentationRevision{}, err
	}
	if result.Revision != revision || result.State.Revision != revision || !c.validPresentation(result.State) || !validID(result.ActorID) || !validJobProgressTime(result.CreatedAt) || result.CreatedAt != result.State.UpdatedAt {
		return PresentationRevision{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ReplacePresentation(ctx context.Context, key string, request ReplacePresentationRequest) (PresentationReceipt, error) {
	if !validMutationKey(key) {
		return PresentationReceipt{}, mutationValidation()
	}
	if err := ValidateReplacePresentationRequest(request); err != nil {
		return PresentationReceipt{}, err
	}
	request.Presentation, _ = presentationmodel.Normalize(request.Presentation)
	var result PresentationReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("presentation"), key, request, http.StatusOK, &result); err != nil {
		return PresentationReceipt{}, err
	}
	if result.ProjectID != c.identity.ProjectID || !(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return PresentationReceipt{}, unknownMutation("invalid_response", "The service returned an invalid presentation receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
