package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxSubagentDocument = 256 << 10

type SubagentRecordRequest struct {
	PullNumber     int64                  `json:"pull_number"`
	HeadSHA        string                 `json:"head_sha"`
	Reviewer       string                 `json:"reviewer"`
	Scope          string                 `json:"scope"`
	StartedAt      string                 `json:"started_at"`
	FinishedAt     string                 `json:"finished_at"`
	CoverageLimits []string               `json:"coverage_limits,omitempty"`
	Findings       []SubagentFindingInput `json:"findings"`
	Finalized      bool                   `json:"finalized,omitempty"`
}
type SubagentFindingInput struct {
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Severity    string `json:"severity"`
	Disposition string `json:"disposition,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
type SubagentDecisionInput struct {
	Finding     int64  `json:"finding"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}
type SubagentFixInput struct {
	Finding     int64   `json:"finding"`
	ExpectedSHA *string `json:"expected_sha"`
	FixSHA      string  `json:"fix_sha"`
	ProofNote   string  `json:"proof_note"`
}
type SubagentUpdateRequest struct {
	ExpectedRevision *int64                  `json:"expected_revision"`
	Decisions        []SubagentDecisionInput `json:"decisions,omitempty"`
	Fixes            []SubagentFixInput      `json:"fixes,omitempty"`
	Finalize         bool                    `json:"finalize,omitempty"`
}
type SubagentReceipt struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}
type SubagentReport struct {
	ID         string           `json:"id"`
	ProjectID  string           `json:"project_id"`
	PullNumber int64            `json:"pull_number"`
	Revision   int64            `json:"revision"`
	Run        ReviewRunSummary `json:"run"`
	Findings   []ReviewFinding  `json:"findings"`
}

func ValidateSubagentRunID(id string) error {
	if !validReviewRunID(id) {
		return validation()
	}
	return nil
}

// Field names and requirements are code-owned; private request values never
// enter a diagnostic, including when malformed provider text supplied a value.
func subagentFieldValidation(field, requirement string) error {
	return fail("validation_failed", "Invalid subagent review field: "+field+".", requirement+" Correct the request before retrying with the retained key.", 400)
}
func ValidateSubagentRecordRequest(in SubagentRecordRequest) error {
	if !validPullNumber(in.PullNumber) {
		return subagentFieldValidation("pull_number", "Use an integer from 1 to 2147483647.")
	}
	if !controlHex(in.HeadSHA, 40) {
		return subagentFieldValidation("head_sha", "Use the full reviewed SHA: 40 lowercase hexadecimal characters.")
	}
	if !controlText(in.Reviewer, 128, true) {
		return subagentFieldValidation("reviewer", "Use nonblank UTF-8 text of at most 128 bytes.")
	}
	if !controlText(in.Scope, 128, true) {
		return subagentFieldValidation("scope", "Use a nonblank comparison label of at most 128 UTF-8 bytes; put longer limitations in coverage_limits.")
	}
	if !validTimestamp(in.StartedAt) {
		return subagentFieldValidation("started_at", "Use an RFC3339 timestamp.")
	}
	if !validTimestamp(in.FinishedAt) {
		return subagentFieldValidation("finished_at", "Use an RFC3339 timestamp.")
	}
	started, _ := time.Parse(time.RFC3339Nano, in.StartedAt)
	finished, _ := time.Parse(time.RFC3339Nano, in.FinishedAt)
	if finished.Before(started) {
		return subagentFieldValidation("finished_at", "Use a timestamp at or after started_at.")
	}
	if in.Findings == nil || len(in.Findings) > 100 {
		return subagentFieldValidation("findings", "Supply an array of at most 100 findings; use [] for a clean review.")
	}
	if len(in.CoverageLimits) > 16 {
		return subagentFieldValidation("coverage_limits", "Use an array of at most 16 nonblank strings, each at most 1024 UTF-8 bytes.")
	}
	for i, limit := range in.CoverageLimits {
		if !controlText(limit, 1024, true) {
			return subagentFieldValidation(fmt.Sprintf("coverage_limits[%d]", i), "Use nonblank UTF-8 text of at most 1024 bytes.")
		}
	}
	for i, f := range in.Findings {
		field := func(name, requirement string) error {
			return subagentFieldValidation(fmt.Sprintf("findings[%d].%s", i, name), requirement)
		}
		if !controlText(f.Title, 500, true) {
			return field("title", "Use nonblank UTF-8 text of at most 500 bytes.")
		}
		if !controlText(f.Detail, 16<<10, false) {
			return field("detail", "Use UTF-8 Markdown of at most 16384 bytes; empty is allowed.")
		}
		if !oneReviewOf(f.Severity, "info", "low", "medium", "high", "critical", "unknown") {
			return field("severity", "Use info, low, medium, high, critical or unknown.")
		}
		if !controlText(f.Reason, 2048, false) {
			return field("reason", "Use UTF-8 text of at most 2048 bytes.")
		}
		switch f.Disposition {
		case "", "unadjudicated":
			if f.Reason != "" {
				return field("reason", "Leave reason empty until the finding is accepted or rejected.")
			}
			if in.Finalized {
				return field("disposition", "Accept or reject each finding before finalized:true.")
			}
		case "accepted", "rejected":
			if !controlText(f.Reason, 2048, true) {
				return field("reason", "Supply a nonblank decision reason of at most 2048 UTF-8 bytes.")
			}
		default:
			return field("disposition", "Use unadjudicated, accepted or rejected.")
		}
	}
	body, err := json.Marshal(in)
	if err != nil || len(body) > maxSubagentDocument {
		return subagentFieldValidation("document", "Use one JSON object of at most 256 KiB.")
	}
	return nil
}
func ValidateSubagentUpdateRequest(in SubagentUpdateRequest) error {
	if in.ExpectedRevision == nil || *in.ExpectedRevision < 1 || *in.ExpectedRevision >= math.MaxInt64 || len(in.Decisions) > 100 || len(in.Fixes) > 100 || len(in.Decisions)+len(in.Fixes) == 0 && !in.Finalize {
		return mutationValidation()
	}
	seen := map[int64]bool{}
	for _, d := range in.Decisions {
		if d.Finding < 0 || d.Finding >= 100 || seen[d.Finding] || !oneReviewOf(d.Disposition, "accepted", "rejected") || !controlText(d.Reason, 2048, true) {
			return mutationValidation()
		}
		seen[d.Finding] = true
	}
	seen = map[int64]bool{}
	for _, f := range in.Fixes {
		if f.Finding < 0 || f.Finding >= 100 || seen[f.Finding] || f.ExpectedSHA == nil || *f.ExpectedSHA != "" && !controlHex(*f.ExpectedSHA, 40) || !controlHex(f.FixSHA, 40) || !controlText(f.ProofNote, 2048, true) {
			return mutationValidation()
		}
		seen[f.Finding] = true
	}
	body, err := json.Marshal(in)
	if err != nil || len(body) > maxSubagentDocument {
		return mutationValidation()
	}
	return nil
}

// DecodeSubagentRecordDocument admits strict required fields before connection
// setup. In particular omitted index zero and omitted empty prior SHA are not
// accepted as if the caller supplied them deliberately.
func DecodeSubagentRecordDocument(document []byte) (SubagentRecordRequest, error) {
	var value SubagentRecordRequest
	if !subagentDocument(document, &value, "pull_number", "head_sha", "reviewer", "scope", "started_at", "finished_at", "findings") {
		return value, subagentRecordShapeError(document)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(document, &fields)
	var items []json.RawMessage
	_ = json.Unmarshal(fields["findings"], &items)
	for _, item := range items {
		if !subagentRequired(item, "title", "detail", "severity") {
			return value, mutationValidation()
		}
	}
	return value, ValidateSubagentRecordRequest(value)
}

// Explain declared top-level shape failures without echoing unknown field names
// or relaxing strict duplicate/unknown/null admission.
func subagentRecordShapeError(document []byte) error {
	if len(document) > 0 && len(document) <= maxSubagentDocument && strictjson.Validate(document) == nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(document, &fields) == nil && fields != nil {
			for _, name := range []string{"pull_number", "head_sha", "reviewer", "scope", "started_at", "finished_at", "findings"} {
				if _, ok := fields[name]; !ok {
					return subagentFieldValidation(name, "Supply this required field; see docs/subagent-reviews.md.")
				}
			}
			if raw, ok := fields["coverage_limits"]; ok {
				var limits []string
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &limits) != nil {
					return subagentFieldValidation("coverage_limits", "Use an array of at most 16 nonblank strings, each at most 1024 UTF-8 bytes; omit it when unused.")
				}
			}
		}
	}
	return subagentFieldValidation("document", "Use one strict JSON object with declared, correctly typed fields and no duplicate keys or null values; see docs/subagent-reviews.md.")
}
func DecodeSubagentUpdateDocument(document []byte) (SubagentUpdateRequest, error) {
	var value SubagentUpdateRequest
	if !subagentDocument(document, &value, "expected_revision") {
		return value, mutationValidation()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(document, &fields)
	for _, name := range []string{"decisions", "fixes"} {
		var items []json.RawMessage
		_ = json.Unmarshal(fields[name], &items)
		for _, item := range items {
			required := []string{"finding", "disposition", "reason"}
			if name == "fixes" {
				required = []string{"finding", "expected_sha", "fix_sha", "proof_note"}
			}
			if !subagentRequired(item, required...) {
				return value, mutationValidation()
			}
		}
	}
	return value, ValidateSubagentUpdateRequest(value)
}
func subagentDocument(document []byte, dst any, required ...string) bool {
	if len(document) == 0 || len(document) > maxSubagentDocument || strictjson.Decode(document, dst) != nil || !subagentRequired(document, required...) {
		return false
	}
	var value any
	if json.Unmarshal(document, &value) != nil {
		return false
	}
	var nonnull func(any) bool
	nonnull = func(v any) bool {
		switch v := v.(type) {
		case nil:
			return false
		case map[string]any:
			for _, item := range v {
				if !nonnull(item) {
					return false
				}
			}
		case []any:
			for _, item := range v {
				if !nonnull(item) {
					return false
				}
			}
		}
		return true
	}
	return nonnull(value)
}
func subagentRequired(document []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(document, &fields) != nil || fields == nil {
		return false
	}
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func (c *Client) RecordSubagentReview(ctx context.Context, in SubagentRecordRequest, key string) (SubagentReceipt, error) {
	if err := ValidateSubagentRecordRequest(in); err != nil {
		return SubagentReceipt{}, err
	}
	return c.subagentMutation(ctx, c.projectPath("reviews", "subagent-reports"), in, key, "recorded", "", 0, http.StatusCreated)
}
func (c *Client) UpdateSubagentReview(ctx context.Context, id string, in SubagentUpdateRequest, key string) (SubagentReceipt, error) {
	if ValidateSubagentRunID(id) != nil {
		return SubagentReceipt{}, mutationValidation()
	}
	if err := ValidateSubagentUpdateRequest(in); err != nil {
		return SubagentReceipt{}, err
	}
	return c.subagentMutation(ctx, c.projectPath("reviews", "subagent-reports", id, "updates"), in, key, "updated", id, *in.ExpectedRevision, http.StatusOK)
}
func (c *Client) subagentMutation(ctx context.Context, path []string, in any, key, state, id string, minimum int64, status int) (SubagentReceipt, error) {
	if !validMutationKey(key) {
		return SubagentReceipt{}, mutationValidation()
	}
	var out SubagentReceipt
	body, err := json.Marshal(in)
	if err != nil {
		return out, mutationValidation()
	}
	if err = c.requestBounded(ctx, http.MethodPost, path, nil, true, key, body, status, true, &out, 16<<10, decodeResponse); err != nil {
		return SubagentReceipt{}, err
	}
	if !validReviewRunID(out.ID) || id != "" && out.ID != id || out.State != state || out.Revision < 1 || out.Revision < minimum || out.Revision >= math.MaxInt64 {
		return SubagentReceipt{}, unknownMutation("invalid_response", "The service returned an invalid subagent review receipt; its outcome is unknown.", status)
	}
	return out, nil
}
func (c *Client) ReadSubagentReview(ctx context.Context, id string) (SubagentReport, error) {
	if err := ValidateSubagentRunID(id); err != nil {
		return SubagentReport{}, err
	}
	var out SubagentReport
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "subagent-reports", id), nil, true, "", nil, http.StatusOK, false, &out, 4<<20, decodeReviewResponse); err != nil {
		return out, err
	}
	if out.ID != id || out.ProjectID != c.identity.ProjectID || !validPullNumber(out.PullNumber) || out.Revision < 1 || out.Revision >= math.MaxInt64 || !validReviewRunSummary(out.Run) || !controlHex(out.Run.HeadSHA, 40) || !out.Run.Attributable || !out.Run.Complete || out.Run.Outcome != "completed" || out.Run.Routed != 0 || !controlText(out.Run.Reviewer, 128, true) || out.Run.Source != "worker-subagent" || out.Run.Channel != "subagent" || out.Run.ID != id || out.Run.PullNumber != out.PullNumber || out.Findings == nil || len(out.Findings) > 100 || int64(len(out.Findings)) != out.Run.Found {
		return SubagentReport{}, invalidResponse()
	}
	seen := map[string]bool{}
	var fixed int64
	for _, f := range out.Findings {
		if !validReviewFinding(f) || seen[f.SourceFindingID] || f.Route != nil || f.Disposition == "routed" {
			return SubagentReport{}, invalidResponse()
		}
		seen[f.SourceFindingID] = true
		if f.FixSHA != "" {
			fixed++
		}
	}
	if fixed != out.Run.Fixed {
		return SubagentReport{}, invalidResponse()
	}
	return out, nil
}
