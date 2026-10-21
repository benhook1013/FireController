package client

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"

	"github.com/benhook1013/FireController/internal/publicsitemodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const MaxPublicationDocumentBytes = 256 << 10

type PublicationManifest = publicsitemodel.Manifest
type PublicationContent = publicsitemodel.Content

type PublicationInput struct {
	ExpectedRevision int64               `json:"expected_revision"`
	Enabled          bool                `json:"enabled"`
	Manifest         PublicationManifest `json:"manifest"`
}

type PublicationConfiguration struct {
	Revision     int64                `json:"revision"`
	Enabled      bool                 `json:"enabled"`
	Manifest     *PublicationManifest `json:"manifest"`
	RefreshError string               `json:"refresh_error"`
}

type PublicationPreview struct {
	ExpectedRevision int64               `json:"expected_revision"`
	Enabled          bool                `json:"enabled"`
	Manifest         PublicationManifest `json:"manifest"`
	Content          PublicationContent  `json:"content"`
	SHA256           string              `json:"preview_sha256"`
}

type PublicationReceipt struct {
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

func normalizePublicationInput(in PublicationInput) (PublicationInput, error) {
	if in.ExpectedRevision < 0 || in.ExpectedRevision == math.MaxInt64 {
		return PublicationInput{}, mutationValidation()
	}
	manifest, err := publicsitemodel.NormalizeManifest(in.Manifest)
	if err != nil {
		return PublicationInput{}, mutationValidation()
	}
	in.Manifest = manifest
	// Preview must admit only a document that also fits apply with its hash.
	if raw, err := publicationApplyBody(in, "0000000000000000000000000000000000000000000000000000000000000000"); err != nil || len(raw) > MaxPublicationDocumentBytes {
		return PublicationInput{}, mutationValidation()
	}
	return in, nil
}

func publicationApplyBody(in PublicationInput, sha string) ([]byte, error) {
	return json.Marshal(struct {
		ExpectedRevision int64               `json:"expected_revision"`
		Enabled          bool                `json:"enabled"`
		Manifest         PublicationManifest `json:"manifest"`
		SHA256           string              `json:"preview_sha256"`
	}{in.ExpectedRevision, in.Enabled, in.Manifest, sha})
}

// DecodePublicationInput validates the complete approved file before clients
// need credentials or transport. It never substitutes current private content.
func DecodePublicationInput(raw []byte) (PublicationInput, error) {
	var wire struct {
		ExpectedRevision *int64          `json:"expected_revision"`
		Enabled          *bool           `json:"enabled"`
		Manifest         json.RawMessage `json:"manifest"`
	}
	if len(raw) > MaxPublicationDocumentBytes || strictjson.Validate(raw) != nil || json.Unmarshal(raw, &wire) != nil || wire.ExpectedRevision == nil || wire.Enabled == nil {
		return PublicationInput{}, mutationValidation()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return PublicationInput{}, mutationValidation()
	}
	for key := range fields {
		if key != "expected_revision" && key != "enabled" && key != "manifest" {
			return PublicationInput{}, mutationValidation()
		}
	}
	manifest, err := publicsitemodel.DecodeManifest(wire.Manifest)
	if err != nil {
		return PublicationInput{}, mutationValidation()
	}
	return normalizePublicationInput(PublicationInput{*wire.ExpectedRevision, *wire.Enabled, manifest})
}

func ValidatePublicationApply(key, previewSHA string, input PublicationInput) error {
	if !validMutationKey(key) || !controlHex(previewSHA, 64) {
		return mutationValidation()
	}
	_, err := normalizePublicationInput(input)
	return err
}

func ValidatePublicationUnpublish(key string, revision int64) error {
	if !validMutationKey(key) || revision < 0 || revision == math.MaxInt64 {
		return mutationValidation()
	}
	return nil
}

func canonicalPublicationManifest(manifest PublicationManifest) bool {
	normalized, err := publicsitemodel.NormalizeManifest(manifest)
	return err == nil && reflect.DeepEqual(normalized, manifest)
}

func decodeCanonicalPublicationManifest(raw []byte) (PublicationManifest, error) {
	manifest, err := publicsitemodel.DecodeManifest(raw)
	if err != nil {
		return PublicationManifest{}, invalidResponse()
	}
	var retained PublicationManifest
	if json.Unmarshal(raw, &retained) != nil || !reflect.DeepEqual(manifest, retained) {
		return PublicationManifest{}, invalidResponse()
	}
	return manifest, nil
}

func (c *Client) Publication(ctx context.Context) (PublicationConfiguration, error) {
	var result PublicationConfiguration
	decode := func(data []byte, _ any) error {
		// The absent configuration deliberately has manifest:null. Decode that
		// one nullable field separately rather than weakening strict DTO reads.
		var wire struct {
			Revision     *int64  `json:"revision"`
			Enabled      *bool   `json:"enabled"`
			Manifest     any     `json:"manifest"`
			RefreshError *string `json:"refresh_error"`
		}
		if strictjson.Decode(data, &wire) != nil || wire.Revision == nil || wire.Enabled == nil || wire.RefreshError == nil {
			return invalidResponse()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || len(fields) != 4 {
			return invalidResponse()
		}
		result = PublicationConfiguration{Revision: *wire.Revision, Enabled: *wire.Enabled, RefreshError: *wire.RefreshError}
		if !bytes.Equal(bytes.TrimSpace(fields["manifest"]), []byte("null")) {
			manifest, err := decodeCanonicalPublicationManifest(fields["manifest"])
			if err != nil {
				return err
			}
			result.Manifest = &manifest
		}
		return nil
	}
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("publication"), nil, true, "", nil, http.StatusOK, false, &result, MaxPublicationDocumentBytes+1024, decode); err != nil {
		return PublicationConfiguration{}, err
	}
	if result.Revision < 0 || result.RefreshError != "" && result.RefreshError != "projection_unavailable" || result.Revision == 0 && (result.Enabled || result.Manifest != nil || result.RefreshError != "") || result.Revision > 0 && (result.Manifest == nil || !canonicalPublicationManifest(*result.Manifest)) {
		return PublicationConfiguration{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) PreviewPublication(ctx context.Context, input PublicationInput) (PublicationPreview, error) {
	input, err := normalizePublicationInput(input)
	if err != nil {
		return PublicationPreview{}, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return PublicationPreview{}, mutationValidation()
	}
	var result PublicationPreview
	decode := func(data []byte, _ any) error {
		var wire struct {
			ExpectedRevision *int64  `json:"expected_revision"`
			Enabled          *bool   `json:"enabled"`
			Manifest         any     `json:"manifest"`
			Content          any     `json:"content"`
			SHA256           *string `json:"preview_sha256"`
		}
		if strictjson.Decode(data, &wire) != nil || wire.ExpectedRevision == nil || wire.Enabled == nil || wire.SHA256 == nil {
			return invalidResponse()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || len(fields) != 5 {
			return invalidResponse()
		}
		manifest, err := decodeCanonicalPublicationManifest(fields["manifest"])
		if err != nil {
			return err
		}
		content, err := publicsitemodel.DecodeContent(fields["content"])
		if err != nil {
			return invalidResponse()
		}
		result = PublicationPreview{ExpectedRevision: *wire.ExpectedRevision, Enabled: *wire.Enabled, Manifest: manifest, Content: content, SHA256: *wire.SHA256}
		return nil
	}
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("publication", "preview"), nil, true, "", body, http.StatusOK, false, &result, 2*MaxPublicationDocumentBytes+2048, decode); err != nil {
		return PublicationPreview{}, err
	}
	if result.ExpectedRevision != input.ExpectedRevision || result.Enabled != input.Enabled || !reflect.DeepEqual(result.Manifest, input.Manifest) || !reflect.DeepEqual(publicsitemodel.EditorialManifest(result.Content), input.Manifest) || !controlHex(result.SHA256, 64) {
		return PublicationPreview{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ApplyPublication(ctx context.Context, key, previewSHA string, input PublicationInput) (PublicationReceipt, error) {
	if err := ValidatePublicationApply(key, previewSHA, input); err != nil {
		return PublicationReceipt{}, err
	}
	input, _ = normalizePublicationInput(input)
	body, err := publicationApplyBody(input, previewSHA)
	if err != nil {
		return PublicationReceipt{}, mutationValidation()
	}
	var result PublicationReceipt
	if err = c.requestBounded(ctx, http.MethodPut, c.projectPath("publication"), nil, true, key, body, http.StatusOK, true, &result, 1024, decodeReviewResponse); err != nil {
		return PublicationReceipt{}, err
	}
	if !validPublicationReceipt(result, input.ExpectedRevision) {
		return PublicationReceipt{}, unknownMutation("invalid_response", "The publication receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func (c *Client) Unpublish(ctx context.Context, key string, revision int64) (PublicationReceipt, error) {
	if err := ValidatePublicationUnpublish(key, revision); err != nil {
		return PublicationReceipt{}, err
	}
	body, _ := json.Marshal(struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}{revision})
	var result PublicationReceipt
	if err := c.requestBounded(ctx, http.MethodPost, c.projectPath("publication", "unpublish"), nil, true, key, body, http.StatusOK, true, &result, 1024, decodeReviewResponse); err != nil {
		return PublicationReceipt{}, err
	}
	if !validPublicationReceipt(result, revision) {
		return PublicationReceipt{}, unknownMutation("invalid_response", "The unpublish receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func validPublicationReceipt(result PublicationReceipt, expected int64) bool {
	return result.Outcome == "updated" && result.Revision == expected+1 || result.Outcome == "unchanged" && result.Revision == expected
}
