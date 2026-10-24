package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type ReviewTransferCounts struct {
	Pulls    int64 `json:"pulls"`
	Runs     int64 `json:"runs"`
	Attempts int64 `json:"attempts"`
	Findings int64 `json:"findings"`
}
type ReviewTransferPreview struct {
	Version          int64                `json:"version"`
	ProjectID        string               `json:"project_id"`
	RepositoryID     int64                `json:"repository_id"`
	Publisher        string               `json:"publisher"`
	ExpectedRevision int64                `json:"expected_revision"`
	PayloadSHA256    string               `json:"payload_sha256"`
	PreviewSHA256    string               `json:"preview_sha256"`
	Counts           ReviewTransferCounts `json:"counts"`
	ImportedHistory  bool                 `json:"imported_history"`
	NativeCoverage   bool                 `json:"native_coverage"`
	Coverage         string               `json:"coverage"`
}
type ReviewTransferReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}
type transferAdmission struct {
	Version          int64             `json:"version"`
	ExpectedRevision *int64            `json:"expected_revision"`
	RepositoryID     int64             `json:"repository_id"`
	Publisher        string            `json:"publisher"`
	Groups           []json.RawMessage `json:"groups"`
}
type transferPullAdmission struct {
	Diff         *ReviewPullDiff   `json:"diff,omitempty"`
	IsDraft      *bool             `json:"is_draft,omitempty"`
	MergedAt     string            `json:"merged_at,omitempty"`
	Number       int64             `json:"number"`
	Title        string            `json:"title"`
	State        string            `json:"state"`
	HeadSHA      string            `json:"head_sha"`
	BaseRef      string            `json:"base_ref"`
	BaseSHA      string            `json:"base_sha"`
	CIState      string            `json:"ci_state"`
	Mergeability string            `json:"mergeability"`
	Complete     json.RawMessage   `json:"complete"`
	Runs         []json.RawMessage `json:"runs"`
	Attempts     []json.RawMessage `json:"attempts"`
}

func transferValidation() error {
	return fail("validation_failed", "Provide one bounded strict JSON version-one additive review transfer batch.", "Correct the selected file and retry with the same destination binding.", 400)
}

// ValidateReviewTransferDocument checks the envelope, partial-coverage admission,
// selected record counts and same-file apply budget before credentials are read.
// The service owns full nested record and retained destination validation.
func ValidateReviewTransferDocument(document json.RawMessage) error {
	_, _, err := reviewTransferDocument(document)
	return err
}
func reviewTransferDocument(document json.RawMessage) (transferAdmission, ReviewTransferCounts, error) {
	var batch transferAdmission
	var counts ReviewTransferCounts
	if len(document) == 0 || len(document) > maxReviewImportDocument || strictjson.Validate(document) != nil || !decodeTransferObject(document, &batch, []string{"version", "expected_revision", "repository_id", "publisher", "groups"}) || batch.Version != 1 || batch.ExpectedRevision == nil || *batch.ExpectedRevision < 0 || batch.RepositoryID <= 0 || !cleanReviewText(batch.Publisher, 128, true) || len(batch.Groups) < 1 || len(batch.Groups) > 100 {
		return batch, counts, transferValidation()
	}
	sources := map[string]bool{}
	pulls := map[int64]bool{}
	appearances := 0
	for _, rawGroup := range batch.Groups {
		var group struct {
			Source string            `json:"source"`
			Pulls  []json.RawMessage `json:"pulls"`
		}
		if !decodeTransferObject(rawGroup, &group, []string{"source", "pulls"}) {
			return batch, counts, transferValidation()
		}
		if !cleanReviewText(group.Source, 128, true) || sources[group.Source] || len(group.Pulls) < 1 {
			return batch, counts, transferValidation()
		}
		sources[group.Source] = true
		localPulls, runIDs, attemptIDs := map[int64]bool{}, map[string]bool{}, map[string]bool{}
		for _, raw := range group.Pulls {
			var pull transferPullAdmission
			if !decodeTransferObject(raw, &pull, []string{"number", "title", "state", "head_sha", "base_ref", "base_sha", "ci_state", "mergeability", "complete", "runs", "attempts", "diff", "is_draft", "merged_at"}) || !validPullNumber(pull.Number) || localPulls[pull.Number] || !cleanReviewText(pull.Title, 500, true) || !oneReviewOf(pull.State, "open", "merged", "closed", "unknown") || !validReviewSHA(pull.HeadSHA) || !cleanReviewText(pull.BaseRef, 256, false) || !validReviewSHA(pull.BaseSHA) || !oneReviewOf(pull.CIState, "unknown", "pending", "success", "failure", "not_required") || !oneReviewOf(pull.Mergeability, "unknown", "mergeable", "conflicting") || len(pull.Runs) > 100 || len(pull.Attempts) > 100 {
				return batch, counts, transferValidation()
			}
			if !validReviewPullMetadata(ReviewPullSummary{State: pull.State, Diff: pull.Diff, IsDraft: pull.IsDraft, MergedAt: pull.MergedAt}) {
				return batch, counts, transferValidation()
			}
			if len(pull.Complete) > 0 && !bytes.Equal(bytes.TrimSpace(pull.Complete), []byte("false")) {
				return batch, counts, transferValidation()
			}
			localPulls[pull.Number] = true
			pulls[pull.Number] = true
			appearances++
			if appearances > 100 {
				return batch, counts, transferValidation()
			}
			counts.Runs += int64(len(pull.Runs))
			counts.Attempts += int64(len(pull.Attempts))
			for _, rawRun := range pull.Runs {
				var run struct {
					SourceRunID string            `json:"source_run_id"`
					Findings    []json.RawMessage `json:"findings"`
				}
				if json.Unmarshal(rawRun, &run) != nil || !cleanReviewText(run.SourceRunID, 256, true) || runIDs[run.SourceRunID] || len(run.Findings) > 100 {
					return batch, counts, transferValidation()
				}
				runIDs[run.SourceRunID] = true
				counts.Findings += int64(len(run.Findings))
			}
			for _, rawAttempt := range pull.Attempts {
				var attempt struct {
					SourceAttemptID string `json:"source_attempt_id"`
				}
				if json.Unmarshal(rawAttempt, &attempt) != nil || !cleanReviewText(attempt.SourceAttemptID, 256, true) || attemptIDs[attempt.SourceAttemptID] {
					return batch, counts, transferValidation()
				}
				attemptIDs[attempt.SourceAttemptID] = true
			}
			if counts.Runs > 200 || counts.Attempts > 200 || counts.Findings > 1000 {
				return batch, counts, transferValidation()
			}
		}
	}
	counts.Pulls = int64(len(pulls))
	wrapper, err := marshalReviewTransferWrapper("0000000000000000000000000000000000000000000000000000000000000000", document)
	if err != nil || len(wrapper) > maxReviewImportDocument {
		return batch, counts, importWireBudgetValidation()
	}
	return batch, counts, nil
}
func decodeTransferObject(raw []byte, dst any, allowed []string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for field := range fields {
		if !slices.Contains(allowed, field) {
			return false
		}
	}
	return json.Unmarshal(raw, dst) == nil
}
func marshalReviewTransferWrapper(previewSHA string, document json.RawMessage) ([]byte, error) {
	return json.Marshal(struct {
		PreviewSHA256 string          `json:"preview_sha256"`
		Batch         json.RawMessage `json:"batch"`
	}{previewSHA, document})
}
func reviewTransferApplyBody(key, previewSHA string, document json.RawMessage) ([]byte, int64, error) {
	if !validMutationKey(key) || !controlHex(previewSHA, 64) {
		return nil, 0, mutationValidation()
	}
	expected, _, err := reviewTransferDocument(document)
	if err != nil {
		return nil, 0, err
	}
	body, err := marshalReviewTransferWrapper(previewSHA, document)
	if err != nil || len(body) > maxReviewImportDocument {
		return nil, 0, importWireBudgetValidation()
	}
	return body, *expected.ExpectedRevision, nil
}
func ValidateReviewTransferApply(key, previewSHA string, document json.RawMessage) error {
	_, _, err := reviewTransferApplyBody(key, previewSHA, document)
	return err
}
func (c *Client) PreviewReviewTransfer(ctx context.Context, document json.RawMessage) (ReviewTransferPreview, error) {
	expected, counts, err := reviewTransferDocument(document)
	if err != nil {
		return ReviewTransferPreview{}, err
	}
	var result ReviewTransferPreview
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "transfer", "preview"), nil, true, "", document, http.StatusOK, false, &result, 4<<10, decodeReviewResponse); err != nil {
		return ReviewTransferPreview{}, err
	}
	if result.Version != 1 || result.ProjectID != c.identity.ProjectID || result.RepositoryID != expected.RepositoryID || result.Publisher != expected.Publisher || result.ExpectedRevision != *expected.ExpectedRevision || result.Counts != counts || !controlHex(result.PayloadSHA256, 64) || !controlHex(result.PreviewSHA256, 64) || !result.ImportedHistory || result.NativeCoverage || result.Coverage != "partial" {
		return ReviewTransferPreview{}, invalidResponse()
	}
	return result, nil
}
func (c *Client) ApplyReviewTransfer(ctx context.Context, key, previewSHA string, document json.RawMessage) (ReviewTransferReceipt, error) {
	body, expectedRevision, err := reviewTransferApplyBody(key, previewSHA, document)
	if err != nil {
		return ReviewTransferReceipt{}, err
	}
	var result ReviewTransferReceipt
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "transfer", "apply"), nil, true, key, body, http.StatusCreated, true, &result, 4<<10, decodeReviewResponse); err != nil {
		var refusal *Error
		if errors.As(err, &refusal) && refusal.Code == "import_preview_mismatch" && refusal.Status == http.StatusConflict && !refusal.OutcomeUnknown {
			safe := *refusal
			safe.Action = "Preview the selected batch again and inspect the new preview before applying."
			return ReviewTransferReceipt{}, &safe
		}
		return ReviewTransferReceipt{}, err
	}
	if !validReviewSnapshot(result.ID) || result.Revision < 1 || expectedRevision == math.MaxInt64 || result.Revision != expectedRevision+1 || result.Outcome != "transferred" {
		return ReviewTransferReceipt{}, unknownMutation("invalid_response", "The service returned an invalid transfer receipt; its outcome is unknown.", http.StatusCreated)
	}
	return result, nil
}
