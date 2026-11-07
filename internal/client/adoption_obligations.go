package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strconv"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type AdoptionObligation = adoptionholdsmodel.Detail
type AdoptionObligationItem = adoptionholdsmodel.ListItem
type AdoptionObligationPage = adoptionholdsmodel.Page
type AdoptionObligationPreview = adoptionholdsmodel.Preview
type AdoptionObligationReceipt = adoptionholdsmodel.Receipt
type AdoptionObligationResolveRequest = adoptionholdsmodel.ResolveRequest

type AdoptionObligationResolutionReceipt struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
}

func adoptionObligationValidation() error {
	return fail("validation_failed", "Provide one bounded strict carried-obligation request.", "Correct the selected arguments or private batch before loading credentials.", http.StatusBadRequest)
}

func adoptionObligationDocument(document json.RawMessage) (adoptionholdsmodel.Batch, []byte, error) {
	var batch adoptionholdsmodel.Batch
	if len(document) == 0 || len(document) > 256<<10 || strictjson.Decode(document, &batch) != nil {
		return batch, nil, adoptionObligationValidation()
	}
	normalized, canonical, err := adoptionholdsmodel.NormalizeBatch(batch)
	if err != nil {
		return batch, nil, adoptionObligationValidation()
	}
	return normalized, canonical, nil
}

// NormalizeAdoptionObligationDocument validates and canonicalizes a private
// batch before profile or credential access.
func NormalizeAdoptionObligationDocument(document json.RawMessage) (json.RawMessage, error) {
	_, canonical, err := adoptionObligationDocument(document)
	return canonical, err
}

func ValidateAdoptionObligationApply(key, previewSHA string, document json.RawMessage) error {
	if !validMutationKey(key) || !controlHex(previewSHA, 64) {
		return mutationValidation()
	}
	_, _, err := adoptionObligationDocument(document)
	return err
}

func NormalizeAdoptionObligationResolution(request AdoptionObligationResolveRequest) (AdoptionObligationResolveRequest, error) {
	normalized, err := adoptionholdsmodel.NormalizeResolution(request)
	if err != nil {
		return request, mutationValidation()
	}
	return normalized, nil
}

func (c *Client) AdoptionObligations(ctx context.Context, pullNumber int64, status string, options PageOptions) (AdoptionObligationPage, error) {
	if pullNumber != 0 && !validPullNumber(pullNumber) || status != "" && status != "open" && status != "resolved" {
		return AdoptionObligationPage{}, validation()
	}
	query, err := pageQuery(options)
	if err != nil || options.Cursor != "" && !validReviewCursor(options.Cursor) {
		return AdoptionObligationPage{}, validation()
	}
	if pullNumber != 0 {
		query.Set("pull_number", strconv.FormatInt(pullNumber, 10))
	}
	if status != "" {
		query.Set("status", status)
	}
	var result AdoptionObligationPage
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "adoption-obligations"), query, true, "", nil, http.StatusOK, false, &result, 256<<10, decodeReviewResponse); err != nil {
		return AdoptionObligationPage{}, err
	}
	limit := options.Limit
	if limit == 0 {
		limit = 100
	}
	if result.Items == nil || len(result.Items) > limit || result.NextCursor != "" && !validReviewCursor(result.NextCursor) {
		return AdoptionObligationPage{}, invalidResponse()
	}
	seen := map[string]bool{}
	effectiveStatus := status
	if effectiveStatus == "" {
		effectiveStatus = "open"
	}
	for _, item := range result.Items {
		if seen[item.ID] || !validAdoptionObligationItem(item) || pullNumber != 0 && !containsPull(item.AffectedPullNumbers, pullNumber) || item.Status != effectiveStatus {
			return AdoptionObligationPage{}, invalidResponse()
		}
		seen[item.ID] = true
	}
	return result, nil
}

// adoptionObligationDetailLimit preserves accepted original (32 KiB), resolution
// (16 KiB) and retarget (16 KiB) records plus bounded attribution and current
// scope. See the encoded-size proof in docs/adoption-obligations.md.
const adoptionObligationDetailLimit = 96 << 10

func (c *Client) AdoptionObligation(ctx context.Context, id string) (AdoptionObligation, error) {
	if !validAdoptionObligationID(id) {
		return AdoptionObligation{}, validation()
	}
	var result AdoptionObligation
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "adoption-obligations", id), nil, true, "", nil, http.StatusOK, false, &result, adoptionObligationDetailLimit, decodeAdoptionObligationDetail); err != nil {
		return AdoptionObligation{}, err
	}
	if !c.validAdoptionObligation(result, id) {
		return AdoptionObligation{}, invalidResponse()
	}
	return result, nil
}

func decodeAdoptionObligationDetail(data []byte, dst any) error {
	value, ok := dst.(*AdoptionObligation)
	if !ok {
		return strictjson.ErrSyntax
	}
	var wire struct {
		ID                  string                        `json:"id"`
		ProjectID           string                        `json:"project_id"`
		Source              adoptionholdsmodel.Source     `json:"source"`
		SourceRevision      int64                         `json:"source_revision"`
		Original            adoptionholdsmodel.Obligation `json:"original"`
		Status              string                        `json:"status"`
		Revision            int64                         `json:"revision"`
		ActorID             string                        `json:"actor_id"`
		WorkerID            string                        `json:"worker_id"`
		CreatedAt           string                        `json:"created_at"`
		Resolution          json.RawMessage               `json:"resolution"`
		AffectedPullNumbers []int64                       `json:"affected_pull_numbers"`
		Retarget            *adoptionholdsmodel.Retarget  `json:"retarget,omitempty"`
	}
	if strictjson.Decode(data, &wire) != nil {
		return strictjson.ErrSyntax
	}
	var resolution *adoptionholdsmodel.Resolution
	if string(wire.Resolution) != "null" {
		var decoded adoptionholdsmodel.Resolution
		if strictjson.Decode(wire.Resolution, &decoded) != nil {
			return strictjson.ErrSyntax
		}
		resolution = &decoded
	}

	*value = AdoptionObligation{AffectedPullNumbers: wire.AffectedPullNumbers, Retarget: wire.Retarget, ID: wire.ID, ProjectID: wire.ProjectID, Source: wire.Source, SourceRevision: wire.SourceRevision, Original: wire.Original, Status: wire.Status, Revision: wire.Revision, ActorID: wire.ActorID, WorkerID: wire.WorkerID, CreatedAt: wire.CreatedAt, Resolution: resolution}
	return nil
}

func (c *Client) PreviewAdoptionObligations(ctx context.Context, document json.RawMessage) (AdoptionObligationPreview, error) {
	batch, canonical, err := adoptionObligationDocument(document)
	if err != nil {
		return AdoptionObligationPreview{}, err
	}
	var result AdoptionObligationPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "adoption-obligations", "preview"), nil, true, "", canonical, http.StatusOK, false, &result, 8<<10, decodeReviewResponse); err != nil {
		return AdoptionObligationPreview{}, err
	}
	if result.ProjectID != c.identity.ProjectID || result.SourceRevision != *batch.ExpectedRevision || result.Selected != len(batch.Obligations) || result.New < 0 || result.Unchanged < 0 || result.New+result.Unchanged != result.Selected || result.PayloadSHA256 != adoptionholdsmodel.Digest(canonical) || !controlHex(result.PreviewSHA256, 64) {
		return AdoptionObligationPreview{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ApplyAdoptionObligations(ctx context.Context, key, previewSHA string, document json.RawMessage) (AdoptionObligationReceipt, error) {
	if !validMutationKey(key) || !controlHex(previewSHA, 64) {
		return AdoptionObligationReceipt{}, mutationValidation()
	}
	batch, _, err := adoptionObligationDocument(document)
	if err != nil {
		return AdoptionObligationReceipt{}, err
	}
	body, err := json.Marshal(adoptionholdsmodel.ApplyRequest{Batch: batch, PreviewSHA256: previewSHA})
	if err != nil || len(body) > 256<<10 {
		return AdoptionObligationReceipt{}, adoptionObligationValidation()
	}
	var result AdoptionObligationReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "adoption-obligations", "apply"), nil, true, key, body, http.StatusOK, true, &result, 8<<10, decodeReviewResponse); err != nil {
		return AdoptionObligationReceipt{}, err
	}
	expected := *batch.ExpectedRevision
	valid := result.New >= 0 && result.Unchanged >= 0 && result.New+result.Unchanged == len(batch.Obligations)
	valid = valid && (result.Outcome == "unchanged" && result.New == 0 && result.Revision == expected || result.Outcome == "imported" && result.New > 0 && expected < math.MaxInt64 && result.Revision == expected+1)
	if !valid {
		return AdoptionObligationReceipt{}, unknownMutation("invalid_response", "The obligation adoption receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func (c *Client) ResolveAdoptionObligation(ctx context.Context, id, key string, request AdoptionObligationResolveRequest) (AdoptionObligationResolutionReceipt, error) {
	if !validAdoptionObligationID(id) || !validMutationKey(key) {
		return AdoptionObligationResolutionReceipt{}, mutationValidation()
	}
	normalized, err := NormalizeAdoptionObligationResolution(request)
	if err != nil {
		return AdoptionObligationResolutionReceipt{}, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return AdoptionObligationResolutionReceipt{}, mutationValidation()
	}
	var result AdoptionObligationResolutionReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "adoption-obligations", id, "resolve"), nil, true, key, body, http.StatusOK, true, &result, 4<<10, decodeReviewResponse); err != nil {
		return AdoptionObligationResolutionReceipt{}, err
	}
	if result.ID != id || result.Status != "resolved" || result.Revision != *normalized.ExpectedRevision+1 {
		return AdoptionObligationResolutionReceipt{}, unknownMutation("invalid_response", "The obligation resolution receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func validAdoptionObligationID(id string) bool {
	return validReviewOpaqueID(id, "adoption_obligation_")
}

func ValidateAdoptionObligationID(id string) error {
	if !validAdoptionObligationID(id) {
		return validation()
	}
	return nil
}

func validAdoptionObligationItem(item AdoptionObligationItem) bool {
	if !validAdoptionObligationID(item.ID) || !adoptionholdsmodel.ValidSourceKey(item.Namespace) || !adoptionholdsmodel.ValidSourceKey(item.SourceKey) || !adoptionholdsmodel.ValidKind(item.Kind) || item.Status != "open" && item.Status != "resolved" || item.Revision < 1 || item.Status == "resolved" && item.Revision < 2 || item.AffectedPullNumbers == nil || len(item.AffectedPullNumbers) > 101 || item.Kind != "route" && len(item.AffectedPullNumbers) > 100 || item.Kind != "route" && (len(item.AffectedPullNumbers) < 1 || item.Status == "open" && item.Revision != 1 || item.Status == "resolved" && item.Revision != 2) {
		return false
	}
	for index, number := range item.AffectedPullNumbers {
		if !adoptionholdsmodel.ValidPull(number) || index > 0 && number <= item.AffectedPullNumbers[index-1] {
			return false
		}
	}
	return true
}

func containsPull(numbers []int64, selected int64) bool {
	for _, number := range numbers {
		if number == selected {
			return true
		}
	}
	return false
}

func (c *Client) validAdoptionObligation(detail AdoptionObligation, id string) bool {
	normalized, err := adoptionholdsmodel.NormalizeObligation(detail.Original)
	if err != nil || !reflect.DeepEqual(normalized, detail.Original) || detail.ID != id || detail.ProjectID != c.identity.ProjectID || !adoptionholdsmodel.ValidSource(detail.Source) || detail.SourceRevision < 1 || detail.Original.SourceKey == "" || !validID(detail.ActorID) || !validID(detail.WorkerID) || !validTimestamp(detail.CreatedAt) {
		return false
	}
	item := AdoptionObligationItem{ID: detail.ID, Namespace: detail.Source.Namespace, SourceKey: detail.Original.SourceKey, Kind: detail.Original.Kind, Status: detail.Status, Revision: detail.Revision, AffectedPullNumbers: detail.AffectedPullNumbers}
	if !validAdoptionObligationItem(item) {
		return false
	}
	if detail.Retarget == nil {
		if !reflect.DeepEqual(detail.AffectedPullNumbers, detail.Original.AffectedPullNumbers) || detail.Status == "open" && detail.Revision != 1 || detail.Status == "resolved" && detail.Revision != 2 {
			return false
		}
	} else {
		retarget := detail.Retarget
		expected := detail.Revision
		if detail.Status == "resolved" {
			expected--
		}
		if detail.Original.Kind != "route" || retarget.Revision < 2 || retarget.Revision != expected || !adoptionholdsmodel.ValidText(retarget.Reason, 4096) || !validID(retarget.ActorID) || !validID(retarget.WorkerID) || !validTimestamp(retarget.ChangedAt) || retarget.TargetPullNumber < 0 || retarget.TargetPullNumber > 2147483647 || !reflect.DeepEqual(detail.AffectedPullNumbers, adoptionholdsmodel.RouteAffectedPullNumbers(detail.Original, retarget.TargetPullNumber)) {
			return false
		}
	}
	if detail.Status == "open" {
		return detail.Resolution == nil
	}
	resolution := detail.Resolution
	if resolution == nil || resolution.Revision != detail.Revision || resolution.Reason == "" || !adoptionholdsmodel.ValidText(resolution.Reason, 4096) || !adoptionholdsmodel.ValidResolutionEvidence(resolution.Evidence) || !validID(resolution.ActorID) || !validID(resolution.WorkerID) || !validTimestamp(resolution.ResolvedAt) {
		return false
	}
	return true
}
