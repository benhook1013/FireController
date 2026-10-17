package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type CoordinationImportBatch = coordimportmodel.Batch
type CoordinationImportPreview = coordimportmodel.Preview
type CoordinationImportReceipt = coordimportmodel.Receipt
type CoordinationImportCounts = coordimportmodel.Counts
type CoordinationImportMapping = coordimportmodel.Mapping

type CoordinationImportState struct {
	Revision int64 `json:"revision"`
}

func coordinationImportInvalid() error {
	return fail("validation_failed", "Provide one bounded strict version-one prepared coordination import batch.", "Correct the selected file locally before previewing or applying it.", 400)
}

// coordinationImportDocument checks local envelope and collection bounds. Native
// domain semantics and retained destination conflicts remain service-owned.
func coordinationImportDocument(raw json.RawMessage) (CoordinationImportBatch, CoordinationImportCounts, string, error) {
	var batch CoordinationImportBatch
	var counts CoordinationImportCounts
	if len(raw) == 0 || len(raw) > 256<<10 || strictjson.Decode(raw, &batch) != nil {
		return batch, counts, "", coordinationImportInvalid()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, name := range []string{"version", "repository_id", "expected_revision", "source", "source_sha256", "jobs"} {
		if len(fields[name]) == 0 || bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return batch, counts, "", coordinationImportInvalid()
		}
	}
	if batch.Version != 1 || batch.RepositoryID <= 0 || batch.ExpectedRevision == nil || *batch.ExpectedRevision < 0 || *batch.ExpectedRevision == math.MaxInt64 ||
		batch.Source == "" || batch.Source != strings.TrimSpace(batch.Source) || len(batch.Source) > 200 || !utf8.ValidString(batch.Source) ||
		strings.IndexFunc(batch.Source, unicode.IsControl) >= 0 || !controlHex(batch.SourceSHA256, 64) || len(batch.Jobs) < 1 || len(batch.Jobs) > 25 {
		return batch, counts, "", coordinationImportInvalid()
	}
	keys := make(map[string]bool, len(batch.Jobs))
	counts.Jobs = len(batch.Jobs)
	for i := range batch.Jobs {
		item := &batch.Jobs[i]
		if strings.TrimSpace(item.SourceKey) == "" || len(item.SourceKey) > 200 || !utf8.ValidString(item.SourceKey) || keys[item.SourceKey] || item.SourceRevision < 1 || len(item.Checkpoints) > 100 {
			return batch, counts, "", coordinationImportInvalid()
		}
		keys[item.SourceKey] = true
		counts.Checkpoints += len(item.Checkpoints)
		if counts.Checkpoints > 100 {
			return batch, counts, "", coordinationImportInvalid()
		}
		if item.Checkpoints == nil {
			item.Checkpoints = []coordimportmodel.PreparedCheckpoint{}
		}
		for j := range item.Checkpoints {
			if item.Checkpoints[j].Evidence == nil {
				item.Checkpoints[j].Evidence = []string{}
			}
		}
	}
	canonical, err := json.Marshal(batch)
	if err != nil {
		return batch, counts, "", coordinationImportInvalid()
	}
	// Both the original submitted JSON and the service's normalized encoding
	// must fit the eventual apply request, including JSON escaping overhead.
	originalWrapper, err := coordinationImportWrapper(strings.Repeat("0", 64), raw)
	normalizedWrapper, normalizedErr := json.Marshal(coordimportmodel.ApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Batch: batch})
	if err != nil || normalizedErr != nil || len(originalWrapper) > 256<<10 || len(normalizedWrapper) > 256<<10 {
		return batch, counts, "", importWireBudgetValidation()
	}
	digest := sha256.Sum256(canonical)
	return batch, counts, hex.EncodeToString(digest[:]), nil
}

func ValidateCoordinationImportDocument(raw json.RawMessage) error {
	_, _, _, err := coordinationImportDocument(raw)
	return err
}

func coordinationImportWrapper(preview string, raw json.RawMessage) ([]byte, error) {
	return json.Marshal(struct {
		Preview string          `json:"preview_sha256"`
		Batch   json.RawMessage `json:"batch"`
	}{preview, raw})
}

func ValidateCoordinationImportApply(key, preview string, raw json.RawMessage) error {
	if !validMutationKey(key) || !controlHex(preview, 64) {
		return mutationValidation()
	}
	return ValidateCoordinationImportDocument(raw)
}

func (c *Client) CoordinationImportState(ctx context.Context) (CoordinationImportState, error) {
	var result CoordinationImportState
	err := c.requestBounded(ctx, http.MethodGet, c.projectPath("coordination", "imports"), nil, true, "", nil, 200, false, &result, 4<<10, decodeReviewResponse)
	if err != nil {
		return CoordinationImportState{}, err
	}
	if result.Revision < 0 {
		return CoordinationImportState{}, invalidResponse()
	}
	return result, nil
}

func validCoordinationCounts(result, expected CoordinationImportCounts) bool {
	return result.Jobs == expected.Jobs && result.Checkpoints == expected.Checkpoints && result.NewJobs >= 0 && result.UnchangedJobs >= 0 &&
		result.NewJobs <= expected.Jobs && result.UnchangedJobs <= expected.Jobs && result.NewJobs+result.UnchangedJobs == expected.Jobs
}

func (c *Client) PreviewCoordinationImport(ctx context.Context, raw json.RawMessage) (CoordinationImportPreview, error) {
	batch, counts, payloadSHA, err := coordinationImportDocument(raw)
	if err != nil {
		return CoordinationImportPreview{}, err
	}
	var result CoordinationImportPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("coordination", "imports", "preview"), nil, true, "", raw, 200, false, &result, 8<<10, decodeReviewResponse); err != nil {
		return CoordinationImportPreview{}, err
	}
	if result.ProjectID != c.identity.ProjectID || result.ExpectedRevision != *batch.ExpectedRevision || result.PayloadSHA256 != payloadSHA ||
		!controlHex(result.PreviewSHA256, 64) || !validCoordinationCounts(result.Counts, counts) {
		return CoordinationImportPreview{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ApplyCoordinationImport(ctx context.Context, key, preview string, raw json.RawMessage) (CoordinationImportReceipt, error) {
	if err := ValidateCoordinationImportApply(key, preview, raw); err != nil {
		return CoordinationImportReceipt{}, err
	}
	batch, counts, _, _ := coordinationImportDocument(raw)
	body, err := coordinationImportWrapper(preview, raw)
	if err != nil {
		return CoordinationImportReceipt{}, coordinationImportInvalid()
	}
	var result CoordinationImportReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("coordination", "imports", "apply"), nil, true, key, body, 200, true, &result, 64<<10, decodeReviewResponse); err != nil {
		return CoordinationImportReceipt{}, err
	}
	valid := result.Revision >= 1 && validCoordinationCounts(result.Counts, counts) && len(result.Mappings) == len(batch.Jobs)
	if result.Counts.NewJobs > 0 {
		valid = valid && result.Outcome == "imported" && result.Revision == *batch.ExpectedRevision+1
	} else {
		valid = valid && result.Outcome == "unchanged" && result.Revision == *batch.ExpectedRevision
	}
	seen := make(map[string]bool, len(result.Mappings))
	for i, mapping := range result.Mappings {
		if i >= len(batch.Jobs) || mapping.SourceKey != batch.Jobs[i].SourceKey || !validID(mapping.JobID) || seen[mapping.JobID] {
			valid = false
		}
		seen[mapping.JobID] = true
	}
	if !valid {
		return CoordinationImportReceipt{}, unknownMutation("invalid_response", "The service returned an invalid import receipt; its outcome is unknown.", 200)
	}
	return result, nil
}
