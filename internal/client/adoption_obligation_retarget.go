package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
)

type AdoptionObligationRetargetRequest = adoptionholdsmodel.RetargetRequest
type AdoptionObligationRetargetReceipt = adoptionholdsmodel.RetargetReceipt

func NormalizeAdoptionObligationRetarget(request AdoptionObligationRetargetRequest) (AdoptionObligationRetargetRequest, error) {
	normalized, err := adoptionholdsmodel.NormalizeRetarget(request)
	if err != nil {
		return request, mutationValidation()
	}
	return normalized, nil
}

func (c *Client) RetargetAdoptionObligation(ctx context.Context, id, key string, request AdoptionObligationRetargetRequest) (AdoptionObligationRetargetReceipt, error) {
	if !validAdoptionObligationID(id) || !validMutationKey(key) {
		return AdoptionObligationRetargetReceipt{}, mutationValidation()
	}
	normalized, err := NormalizeAdoptionObligationRetarget(request)
	if err != nil {
		return AdoptionObligationRetargetReceipt{}, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return AdoptionObligationRetargetReceipt{}, mutationValidation()
	}
	var out AdoptionObligationRetargetReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "adoption-obligations", id, "retarget"), nil, true, key, body, http.StatusOK, true, &out, 8<<10, decodeReviewResponse); err != nil {
		return out, err
	}
	valid := out.ID == id && out.Status == "open" && out.AffectedPullNumbers != nil && out.TargetPullNumber == *normalized.TargetPullNumber
	for index, number := range out.AffectedPullNumbers {
		if !adoptionholdsmodel.ValidPull(number) || index > 0 && number <= out.AffectedPullNumbers[index-1] {
			valid = false
		}
	}
	if len(out.AffectedPullNumbers) > 101 || out.TargetPullNumber > 0 && !containsPull(out.AffectedPullNumbers, out.TargetPullNumber) {
		valid = false
	}
	valid = valid && (out.Outcome == "retargeted" && out.Revision == *normalized.ExpectedRevision+1 || out.Outcome == "unchanged" && out.Revision == *normalized.ExpectedRevision)
	if !valid {
		return AdoptionObligationRetargetReceipt{}, unknownMutation("invalid_response", "The obligation retarget receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return out, nil
}
