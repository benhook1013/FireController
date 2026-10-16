package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxReviewImportDocument = 256 << 10

type ReviewImportCounts struct {
	Pulls       int64 `json:"pulls"`
	Runs        int64 `json:"runs"`
	Attempts    int64 `json:"attempts"`
	Findings    int64 `json:"findings"`
	Retirements int64 `json:"retirements"`
}
type ReviewImportPreview struct {
	Version          int64              `json:"version"`
	ProjectID        string             `json:"project_id"`
	RepositoryID     int64              `json:"repository_id"`
	Source           string             `json:"source"`
	ObservedAt       string             `json:"observed_at"`
	ExpectedRevision int64              `json:"expected_revision"`
	PayloadSHA256    string             `json:"payload_sha256"`
	PreviewSHA256    string             `json:"preview_sha256"`
	Counts           ReviewImportCounts `json:"counts"`
	ImportedHistory  bool               `json:"imported_history"`
	NativeCoverage   bool               `json:"native_coverage"`
}
type ReviewImportReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}
type reviewImportEnvelope struct {
	revision, repository int64
	source, observed     string
	pulls, retirements   int
}

func importValidation() error {
	return fail("validation_failed", "Provide one bounded strict JSON review observation document.", "Correct the selected file and retry with the same destination binding.", 400)
}

// ValidateReviewImportDocument checks bounded JSON and the observation envelope.
// The service owns complete nested record and destination validation.
func ValidateReviewImportDocument(document json.RawMessage) error {
	_, err := reviewImportDocument(document)
	return err
}
func reviewImportDocument(document json.RawMessage) (reviewImportEnvelope, error) {
	var out reviewImportEnvelope
	if len(document) == 0 || len(document) > maxReviewImportDocument || strictjson.Validate(document) != nil {
		return out, importValidation()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(document, &fields) != nil || fields == nil {
		return out, importValidation()
	}
	for key := range fields {
		switch key {
		case "expected_revision", "repository_id", "source", "observed_at", "queue", "retired":
		default:
			return out, importValidation()
		}
	}
	for key, dst := range map[string]any{"expected_revision": &out.revision, "repository_id": &out.repository, "source": &out.source, "observed_at": &out.observed} {
		raw, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, dst) != nil {
			return out, importValidation()
		}
	}
	if out.revision < 0 || out.repository <= 0 || !cleanReviewText(out.source, 128, true) || len(out.observed) > 64 {
		return out, importValidation()
	}
	if _, err := time.Parse(time.RFC3339Nano, out.observed); err != nil {
		return out, importValidation()
	}
	for key, dst := range map[string]*int{"queue": &out.pulls, "retired": &out.retirements} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return out, importValidation()
		}
		*dst = len(items)
	}
	// Budget exactly the compact RawMessage encoding used by Apply. The
	// service may normalize escapes to a smaller document; local preview must
	// still ensure this same file can be sent by the typed apply call.
	wrapper, err := marshalReviewImportWrapper("0000000000000000000000000000000000000000000000000000000000000000", document)
	if err != nil || len(wrapper) > maxReviewImportDocument {
		return out, importWireBudgetValidation()
	}
	return out, nil
}
func importWireBudgetValidation() error {
	return fail("validation_failed", "The selected document exceeds the 256 KiB apply request budget.", "Choose a smaller document that fits both preview and apply before retrying.", 400)
}
func marshalReviewImportWrapper(previewSHA string, document json.RawMessage) ([]byte, error) {
	return json.Marshal(struct {
		PreviewSHA256 string          `json:"preview_sha256"`
		Observation   json.RawMessage `json:"observation"`
	}{previewSHA, document})
}
func reviewImportApplyBody(key, previewSHA string, document json.RawMessage) ([]byte, error) {
	if !validMutationKey(key) || !controlHex(previewSHA, 64) {
		return nil, mutationValidation()
	}
	if err := ValidateReviewImportDocument(document); err != nil {
		return nil, err
	}
	body, err := marshalReviewImportWrapper(previewSHA, document)
	if err != nil || len(body) > maxReviewImportDocument {
		return nil, importWireBudgetValidation()
	}
	return body, nil
}

// ValidateReviewImportApply checks local arguments and the bounded apply wrapper
// before a CLI needs credentials or service access.
func ValidateReviewImportApply(key, previewSHA string, document json.RawMessage) error {
	_, err := reviewImportApplyBody(key, previewSHA, document)
	return err
}
func (c *Client) PreviewReviewImport(ctx context.Context, document json.RawMessage) (ReviewImportPreview, error) {
	expected, err := reviewImportDocument(document)
	if err != nil {
		return ReviewImportPreview{}, err
	}
	var v ReviewImportPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "import", "preview"), nil, true, "", document, http.StatusOK, false, &v, 4<<10, decodeReviewResponse); err != nil {
		return ReviewImportPreview{}, err
	}
	n := v.Counts
	if v.Version != 1 || v.ProjectID != c.identity.ProjectID || v.RepositoryID != expected.repository || v.Source != expected.source || v.ObservedAt != expected.observed || v.ExpectedRevision != expected.revision || !controlHex(v.PayloadSHA256, 64) || !controlHex(v.PreviewSHA256, 64) || !v.ImportedHistory || v.NativeCoverage || n.Pulls < 0 || n.Pulls > 100 || n.Pulls != int64(expected.pulls) || n.Retirements < 0 || n.Retirements > 100 || n.Retirements != int64(expected.retirements) || n.Runs < 0 || n.Runs > 200 || n.Attempts < 0 || n.Attempts > 200 || n.Findings < 0 || n.Findings > 1000 || n.Runs > n.Pulls*100 || n.Attempts > n.Pulls*100 || n.Findings > n.Runs*100 {
		return ReviewImportPreview{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) ApplyReviewImport(ctx context.Context, key, previewSHA string, document json.RawMessage) (ReviewImportReceipt, error) {
	body, err := reviewImportApplyBody(key, previewSHA, document)
	if err != nil {
		return ReviewImportReceipt{}, err
	}
	var v ReviewImportReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "import", "apply"), nil, true, key, body, http.StatusCreated, true, &v, 4<<10, decodeReviewResponse); err != nil {
		var refusal *Error
		if errors.As(err, &refusal) && refusal.Code == "import_preview_mismatch" && refusal.Status == http.StatusConflict && !refusal.OutcomeUnknown {
			safe := *refusal
			safe.Action = "Preview the selected document again and inspect the new preview before applying."
			return ReviewImportReceipt{}, &safe
		}
		return ReviewImportReceipt{}, err
	}
	if !validReviewSnapshot(v.ID) || v.Revision < 1 || v.Outcome != "observed" {
		return ReviewImportReceipt{}, unknownMutation("invalid_response", "The service returned an invalid import receipt; its outcome is unknown.", http.StatusCreated)
	}
	return v, nil
}
