package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type PreparedReviewRoundControl = roundadoptionmodel.Prepared
type ReviewRoundAdoptionPreview = roundadoptionmodel.Preview

func roundAdoptionInvalid() error {
	return fail("validation_failed", "Provide one strict prepared operational review control within 16 KiB.", "Correct the private file locally before previewing or applying it.", 400)
}

func roundAdoptionDocument(raw json.RawMessage) (PreparedReviewRoundControl, string, error) {
	var value PreparedReviewRoundControl
	if len(raw) == 0 || len(raw) > 16<<10 || strictjson.Decode(raw, &value) != nil {
		return value, "", roundAdoptionInvalid()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, name := range []string{"version", "repository_id", "expected_revision", "pull_number", "channel", "source", "source_key", "source_sha256", "minimum_remaining", "maximum_remaining", "initial_dry", "taper", "tapered", "closed"} {
		if len(fields[name]) == 0 || bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return value, "", roundAdoptionInvalid()
		}
	}
	text := func(s string) bool {
		return s != "" && s == strings.TrimSpace(s) && len(s) <= 200 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
	}
	if value.Version != 1 || value.RepositoryID < 1 || value.ExpectedRevision == nil || *value.ExpectedRevision != 0 || !validPullNumber(value.PullNumber) || !oneReviewOf(value.Channel, "cli", "hosted") || !text(value.Source) || !text(value.SourceKey) || !controlHex(value.SourceSHA256, 64) || value.MinimumRemaining < 0 || value.MaximumRemaining < value.MinimumRemaining || value.MaximumRemaining > 1000 || value.InitialDry < 0 || value.InitialDry > 1000 || value.Taper < 1 || value.Taper > 1000 || value.InitialDry >= value.Taper && !value.Tapered {
		return value, "", roundAdoptionInvalid()
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return value, "", roundAdoptionInvalid()
	}
	digest := sha256.Sum256(canonical)
	return value, hex.EncodeToString(digest[:]), nil
}

func ValidateReviewRoundAdoptionDocument(raw json.RawMessage) error {
	_, _, err := roundAdoptionDocument(raw)
	return err
}

func ValidateReviewRoundAdoptionApply(key, preview string, raw json.RawMessage) error {
	if !validMutationKey(key) || !controlHex(preview, 64) {
		return mutationValidation()
	}
	return ValidateReviewRoundAdoptionDocument(raw)
}

func (c *Client) PreviewReviewRoundAdoption(ctx context.Context, raw json.RawMessage) (ReviewRoundAdoptionPreview, error) {
	value, payload, err := roundAdoptionDocument(raw)
	if err != nil {
		return ReviewRoundAdoptionPreview{}, err
	}
	var result ReviewRoundAdoptionPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "rounds", "adoption", "preview"), nil, true, "", raw, http.StatusOK, false, &result, 8<<10, decodeReviewResponse); err != nil {
		return ReviewRoundAdoptionPreview{}, err
	}
	if result.ProjectID != c.identity.ProjectID || result.PullNumber != value.PullNumber || result.Channel != value.Channel || result.PayloadSHA256 != payload || !controlHex(result.PreviewSHA256, 64) {
		return ReviewRoundAdoptionPreview{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ApplyReviewRoundAdoption(ctx context.Context, key, preview string, raw json.RawMessage) (ReviewRoundControlReceipt, error) {
	if err := ValidateReviewRoundAdoptionApply(key, preview, raw); err != nil {
		return ReviewRoundControlReceipt{}, err
	}
	value, _, _ := roundAdoptionDocument(raw)
	body, err := json.Marshal(struct {
		Preview string          `json:"preview_sha256"`
		Control json.RawMessage `json:"control"`
	}{preview, raw})
	if err != nil {
		return ReviewRoundControlReceipt{}, roundAdoptionInvalid()
	}
	var result ReviewRoundControlReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "rounds", "adoption", "apply"), nil, true, key, body, http.StatusOK, true, &result, 8<<10, decodeReviewResponse); err != nil {
		return ReviewRoundControlReceipt{}, err
	}
	if result.PullNumber != value.PullNumber || result.Channel != value.Channel || result.Revision != 1 || result.Epoch != 1 || result.Outcome != "adopted" {
		return ReviewRoundControlReceipt{}, unknownMutation("invalid_response", "The adoption receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
