package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type ReviewAnchor struct {
	RepositoryID     int64  `json:"repository_id"`
	Repository       string `json:"repository"`
	PullNumber       int64  `json:"pull_number"`
	CandidateSHA     string `json:"candidate_sha"`
	BaseSHA          string `json:"base_sha"`
	ParentSHA        string `json:"parent_sha"`
	ParentPullNumber int64  `json:"parent_pull_number"`
	BaseRef          string `json:"base_ref"`
	MergeBaseSHA     string `json:"merge_base_sha"`
	PatchSHA256      string `json:"patch_sha256"`
	ObservedAt       string `json:"observed_at"`
}
type ReviewHostedPublication struct {
	PublicationID     string  `json:"publication_id"`
	ProviderAttemptID string  `json:"provider_attempt_id"`
	TriggerCommentID  int64   `json:"trigger_comment_id"`
	ReviewIDs         []int64 `json:"review_ids"`
	ObservedBaseSHA   string  `json:"observed_base_sha"`
}
type ReviewHostedFindingSource struct {
	SourceKey   string `json:"source_key"`
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind"`
	Side        string `json:"side"`
	ReviewID    int64  `json:"review_id"`
	CommentID   int64  `json:"comment_id"`
}
type ReviewEvidenceFinding struct {
	Finding   int64                      `json:"finding"`
	Source    *ReviewHostedFindingSource `json:"source,omitempty"`
	Title     string                     `json:"title"`
	Detail    string                     `json:"detail"`
	Severity  string                     `json:"severity"`
	Path      string                     `json:"path"`
	StartLine int64                      `json:"start_line"`
	EndLine   int64                      `json:"end_line"`
}
type ReviewEvidenceCapture struct {
	Hosted           *ReviewHostedPublication `json:"hosted,omitempty"`
	CaptureID        string                   `json:"capture_id"`
	CaptureSHA256    string                   `json:"capture_sha256"`
	CaptureBytes     int64                    `json:"capture_bytes"`
	StartedAt        string                   `json:"started_at"`
	FinishedAt       string                   `json:"finished_at"`
	Outcome          string                   `json:"outcome"`
	Complete         bool                     `json:"complete"`
	ReportedFindings int64                    `json:"reported_findings"`
	ReviewedFiles    []string                 `json:"reviewed_files"`
	Findings         []ReviewEvidenceFinding  `json:"findings"`
}
type ReviewEvidence struct {
	Channel               string                `json:"channel"`
	ID                    string                `json:"id"`
	ProjectID             string                `json:"project_id"`
	AttemptID             string                `json:"attempt_id"`
	OperationID           string                `json:"operation_id"`
	CapabilityFingerprint string                `json:"capability_fingerprint"`
	Anchor                ReviewAnchor          `json:"anchor"`
	Capture               ReviewEvidenceCapture `json:"capture"`
	RecordedAt            string                `json:"recorded_at"`
}
type ReviewCounts struct {
	Found    int64 `json:"found"`
	Accepted int64 `json:"accepted"`
	Routed   int64 `json:"routed"`
}
type ReviewFindingDecision struct {
	Finding     int64  `json:"finding"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
	ActorID     string `json:"actor_id"`
	DecidedAt   string `json:"decided_at"`
	FixSHA      string `json:"fix_sha"`
	ProofNote   string `json:"proof_note"`
	FixedAt     string `json:"fixed_at"`
}
type ReviewAdjudication struct {
	EvidenceID   string                  `json:"evidence_id"`
	Revision     int64                   `json:"revision"`
	Finalized    bool                    `json:"finalized"`
	FrozenCounts *ReviewCounts           `json:"frozen_counts"`
	Findings     []ReviewFindingDecision `json:"findings"`
}
type ReviewControlReceipt struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}
type ReviewDecisionRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Finding          int64  `json:"finding"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
}
type ReviewFixRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Finding          int64  `json:"finding"`
	ExpectedSHA      string `json:"expected_sha"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
}
type ReviewFinalizeRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
}

func ValidateReviewEvidenceID(id string) error {
	if !validReviewOpaqueID(id, "review_evidence_") {
		return validation()
	}
	return nil
}
func validControlRevision(revision int64) bool { return revision >= 0 && revision < math.MaxInt64 }
func controlText(value string, max int, required bool) bool {
	return len(value) <= max && utf8.ValidString(value) && (!required || strings.TrimSpace(value) != "") && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) < 0
}
func controlHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func ValidateReviewDecisionRequest(r ReviewDecisionRequest) error {
	if !validControlRevision(r.ExpectedRevision) || r.Finding < 0 || r.Finding >= 100 || !oneReviewOf(r.Disposition, "accepted", "rejected") || !controlText(r.Reason, 2048, true) {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewFixRequest(r ReviewFixRequest) error {
	if !validControlRevision(r.ExpectedRevision) || r.Finding < 0 || r.Finding >= 100 || r.ExpectedSHA != "" && !controlHex(r.ExpectedSHA, 40) || !controlHex(r.FixSHA, 40) || !controlText(r.ProofNote, 2048, true) {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewFinalizeRequest(r ReviewFinalizeRequest) error {
	if !validControlRevision(r.ExpectedRevision) {
		return mutationValidation()
	}
	return nil
}
func controlPath(path string) bool {
	if path == "" || !utf8.ValidString(path) || strings.ContainsAny(path, "\\:") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func controlTime(value string) bool {
	if !validTimestamp(value) {
		return false
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return !parsed.IsZero()
}
func validControlAnchor(a ReviewAnchor) bool {
	parts := strings.Split(a.Repository, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 100 || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				return false
			}
		}
	}
	return a.RepositoryID > 0 && validPullNumber(a.PullNumber) && a.ParentPullNumber >= 0 && a.ParentPullNumber <= 2147483647 && controlHex(a.CandidateSHA, 40) && controlHex(a.BaseSHA, 40) && controlHex(a.ParentSHA, 40) && controlHex(a.MergeBaseSHA, 40) && controlHex(a.PatchSHA256, 64) && controlText(a.BaseRef, 256, true) && controlTime(a.ObservedAt)
}

func (c *Client) ReviewEvidence(ctx context.Context, id string) (ReviewEvidence, error) {
	if err := ValidateReviewEvidenceID(id); err != nil {
		return ReviewEvidence{}, err
	}
	var value ReviewEvidence
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "evidence", id), nil, true, "", nil, http.StatusOK, false, &value, 4<<20, decodeReviewResponse); err != nil {
		return ReviewEvidence{}, err
	}
	r := value.Capture
	captureLimit := int64(24 << 20)
	if value.Channel == "hosted" {
		captureLimit = 48 << 20
	}
	if !oneReviewOf(value.Channel, "cli", "hosted") || value.ID != id || value.ProjectID != c.identity.ProjectID || !validReviewOpaqueID(value.AttemptID, "review_attempt_") || !validReviewOpaqueID(value.OperationID, "review_operation_") || !controlHex(value.CapabilityFingerprint, 64) || !validControlAnchor(value.Anchor) || !controlTime(value.RecordedAt) || !controlHex(r.CaptureSHA256, 64) || r.CaptureID != "review_capture_"+r.CaptureSHA256[:32] || r.CaptureBytes < 1 || r.CaptureBytes > captureLimit || !controlTime(r.StartedAt) || !controlTime(r.FinishedAt) || !oneReviewOf(r.Outcome, "completed", "failed", "cancelled", "rate_limited", "uncertain") || r.Complete != (r.Outcome == "completed") || r.ReportedFindings < 0 || r.Findings == nil || len(r.Findings) > 100 || r.ReviewedFiles == nil || len(r.ReviewedFiles) > 10000 {
		return ReviewEvidence{}, invalidResponse()
	}
	start, _ := time.Parse(time.RFC3339Nano, r.StartedAt)
	finish, _ := time.Parse(time.RFC3339Nano, r.FinishedAt)
	if finish.Before(start) || r.Complete && r.ReportedFindings != int64(len(r.Findings)) {
		return ReviewEvidence{}, invalidResponse()
	}
	if value.Channel == "cli" {
		if r.Hosted != nil || r.Complete && len(r.ReviewedFiles) == 0 {
			return ReviewEvidence{}, invalidResponse()
		}
	} else if !validHostedControlCapture(r, value.Anchor) {
		return ReviewEvidence{}, invalidResponse()
	}
	seen := make(map[string]bool)
	for _, path := range r.ReviewedFiles {
		if !controlPath(path) || r.Complete && seen[path] {
			return ReviewEvidence{}, invalidResponse()
		}
		seen[path] = true
	}
	for i, f := range r.Findings {
		if value.Channel == "cli" && f.Source != nil {
			return ReviewEvidence{}, invalidResponse()
		}
		if f.Finding != int64(i) || !controlText(f.Title, 500, true) || !controlText(f.Detail, 16<<10, false) || !oneReviewOf(f.Severity, "info", "low", "medium", "high", "critical", "unknown") || f.Path != "" && !controlPath(f.Path) || f.StartLine < 0 || f.EndLine < 0 || f.StartLine != 0 && f.EndLine != 0 && f.EndLine < f.StartLine {
			return ReviewEvidence{}, invalidResponse()
		}
	}
	return value, nil
}

func controlUUID(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' && controlHex(value[:8]+value[9:13]+value[14:18]+value[19:23]+value[24:], 32)
}

func validHostedControlCapture(capture ReviewEvidenceCapture, anchor ReviewAnchor) bool {
	hosted := capture.Hosted
	if hosted == nil || !capture.Complete || capture.Outcome != "completed" || len(capture.ReviewedFiles) != 0 || capture.ReportedFindings != int64(len(capture.Findings)) || !controlUUID(hosted.PublicationID) || !controlUUID(hosted.ProviderAttemptID) || hosted.TriggerCommentID <= 0 || len(hosted.ReviewIDs) < 1 || len(hosted.ReviewIDs) > 200 || !controlHex(hosted.ObservedBaseSHA, 40) || hosted.ObservedBaseSHA != anchor.ParentSHA && hosted.ObservedBaseSHA != anchor.MergeBaseSHA {
		return false
	}
	reviews := make(map[int64]bool, len(hosted.ReviewIDs))
	for _, id := range hosted.ReviewIDs {
		if id <= 0 || reviews[id] {
			return false
		}
		reviews[id] = true
	}
	fingerprints := make(map[string]bool, len(capture.Findings))
	for _, finding := range capture.Findings {
		source := finding.Source
		if source == nil || !controlHex(source.Fingerprint, 24) || fingerprints[source.Fingerprint] || !reviews[source.ReviewID] || !oneReviewOf(source.Kind, "inline", "outside_diff", "duplicate") || !oneReviewOf(source.Side, "", "LEFT", "RIGHT") || finding.Path == "" {
			return false
		}
		fingerprints[source.Fingerprint] = true
		if source.Kind == "inline" {
			if source.CommentID <= 0 || source.SourceKey != "hosted-comment:"+strconv.FormatInt(source.CommentID, 10)+":"+source.Fingerprint {
				return false
			}
		} else if source.CommentID != 0 || source.SourceKey != "hosted-review:"+strconv.FormatInt(source.ReviewID, 10)+":"+source.Kind+":"+source.Fingerprint || finding.StartLine <= 0 || finding.EndLine < finding.StartLine {
			return false
		}
	}
	return true
}

func (c *Client) ReviewAdjudication(ctx context.Context, id string) (ReviewAdjudication, error) {
	if err := ValidateReviewEvidenceID(id); err != nil {
		return ReviewAdjudication{}, err
	}
	var value ReviewAdjudication
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "evidence", id, "adjudication"), nil, true, "", nil, http.StatusOK, false, &value, 4<<20, decodeReviewAdjudication); err != nil {
		return ReviewAdjudication{}, err
	}
	if value.EvidenceID != id || value.Revision < 0 || value.Finalized != (value.FrozenCounts != nil) || value.Findings == nil || len(value.Findings) > 100 {
		return ReviewAdjudication{}, invalidResponse()
	}
	if counts := value.FrozenCounts; counts != nil && (value.Revision == 0 || counts.Found != int64(len(value.Findings)) || counts.Accepted < 0 || counts.Routed != 0 || counts.Accepted > counts.Found) {
		return ReviewAdjudication{}, invalidResponse()
	}
	for i, f := range value.Findings {
		if f.Finding != int64(i) || !oneReviewOf(f.Disposition, "unadjudicated", "accepted", "rejected") || !controlText(f.Reason, 2048, false) || !controlText(f.ProofNote, 2048, false) {
			return ReviewAdjudication{}, invalidResponse()
		}
		if f.Disposition == "unadjudicated" {
			if value.Finalized || f.Reason != "" || f.ActorID != "" || f.DecidedAt != "" || f.FixSHA != "" || f.ProofNote != "" || f.FixedAt != "" {
				return ReviewAdjudication{}, invalidResponse()
			}
		} else if value.Revision == 0 || !controlText(f.Reason, 2048, true) || !validID(f.ActorID) || !controlTime(f.DecidedAt) {
			return ReviewAdjudication{}, invalidResponse()
		}
		if f.FixSHA == "" {
			if f.ProofNote != "" || f.FixedAt != "" {
				return ReviewAdjudication{}, invalidResponse()
			}
		} else if f.Disposition != "accepted" || !controlHex(f.FixSHA, 40) || !controlText(f.ProofNote, 2048, true) || !controlTime(f.FixedAt) {
			return ReviewAdjudication{}, invalidResponse()
		}
	}
	return value, nil
}

// Only frozen_counts is nullable; shared strict response decoding stays unchanged.
func decodeReviewAdjudication(data []byte, dst any) error {
	value, ok := dst.(*ReviewAdjudication)
	if !ok || strictjson.Validate(data) != nil {
		return strictjson.ErrSyntax
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 5 {
		return strictjson.ErrSyntax
	}
	for _, name := range []string{"evidence_id", "revision", "finalized", "frozen_counts", "findings"} {
		if _, ok := fields[name]; !ok {
			return strictjson.ErrSyntax
		}
	}
	for name, target := range map[string]any{"evidence_id": &value.EvidenceID, "revision": &value.Revision, "finalized": &value.Finalized, "findings": &value.Findings} {
		if decodeReviewResponse(fields[name], target) != nil {
			return strictjson.ErrSyntax
		}
	}
	value.FrozenCounts = nil
	if !bytes.Equal(bytes.TrimSpace(fields["frozen_counts"]), []byte("null")) {
		var counts ReviewCounts
		if decodeReviewResponse(fields["frozen_counts"], &counts) != nil {
			return strictjson.ErrSyntax
		}
		value.FrozenCounts = &counts
	}
	return nil
}

func (c *Client) DecideReviewFinding(ctx context.Context, key, id string, request ReviewDecisionRequest) (ReviewControlReceipt, error) {
	if err := ValidateReviewDecisionRequest(request); err != nil {
		return ReviewControlReceipt{}, err
	}
	return c.reviewControl(ctx, key, id, "decisions", "adjudicated", request.ExpectedRevision, request)
}
func (c *Client) RecordReviewFix(ctx context.Context, key, id string, request ReviewFixRequest) (ReviewControlReceipt, error) {
	if err := ValidateReviewFixRequest(request); err != nil {
		return ReviewControlReceipt{}, err
	}
	return c.reviewControl(ctx, key, id, "fixes", "fix_recorded", request.ExpectedRevision, request)
}
func (c *Client) FinalizeReviewSource(ctx context.Context, key, id string, request ReviewFinalizeRequest) (ReviewControlReceipt, error) {
	if err := ValidateReviewFinalizeRequest(request); err != nil {
		return ReviewControlReceipt{}, err
	}
	return c.reviewControl(ctx, key, id, "finalization", "finalized", request.ExpectedRevision, request)
}
func (c *Client) reviewControl(ctx context.Context, key, id, suffix, state string, expected int64, request any) (ReviewControlReceipt, error) {
	if !validMutationKey(key) || ValidateReviewEvidenceID(id) != nil {
		return ReviewControlReceipt{}, mutationValidation()
	}
	var result ReviewControlReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "evidence", id, suffix), key, request, http.StatusOK, &result); err != nil {
		return ReviewControlReceipt{}, err
	}
	if result.ID != id || result.State != state || result.Revision != expected+1 {
		return ReviewControlReceipt{}, unknownMutation("invalid_response", "The service returned an invalid review receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
