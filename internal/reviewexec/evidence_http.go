package reviewexec

import (
	"net/http"
	"regexp"

	"github.com/benhook1013/FireController/internal/core"
)

var evidenceHTTPID = regexp.MustCompile(`^review_evidence_[0-9a-f]{32}$`)

// The HTTP representation keeps provider parser implementation fields private
// and exposes each source finding's immutable zero-based index explicitly.
type evidenceResponse struct {
	ID                    string          `json:"id"`
	Channel               string          `json:"channel"`
	ProjectID             string          `json:"project_id"`
	AttemptID             string          `json:"attempt_id"`
	OperationID           string          `json:"operation_id"`
	CapabilityFingerprint string          `json:"capability_fingerprint"`
	Anchor                Anchor          `json:"anchor"`
	Capture               captureResponse `json:"capture"`
	RecordedAt            string          `json:"recorded_at"`
}

type hostedPublicationResponse struct {
	PublicationID     string                         `json:"publication_id"`
	ProviderAttemptID string                         `json:"provider_attempt_id"`
	TriggerCommentID  int64                          `json:"trigger_comment_id"`
	ReviewIDs         []int64                        `json:"review_ids"`
	ObservedBaseSHA   string                         `json:"observed_base_sha"`
	StandingSummary   *HostedStandingSummaryMetadata `json:"standing_summary,omitempty"`
	SubmittedReview   *HostedSubmittedReviewMetadata `json:"submitted_review,omitempty"`
}

type captureResponse struct {
	Hosted           *hostedPublicationResponse `json:"hosted,omitempty"`
	CaptureID        string                     `json:"capture_id"`
	CaptureSHA256    string                     `json:"capture_sha256"`
	CaptureBytes     int64                      `json:"capture_bytes"`
	StartedAt        string                     `json:"started_at"`
	FinishedAt       string                     `json:"finished_at"`
	Outcome          string                     `json:"outcome"`
	Complete         bool                       `json:"complete"`
	ReportedFindings int64                      `json:"reported_findings"`
	ReviewedFiles    []string                   `json:"reviewed_files"`
	Findings         []sourceFindingResponse    `json:"findings"`
}

type sourceFindingResponse struct {
	Source    *HostedFindingSource `json:"source,omitempty"`
	Finding   int64                `json:"finding"`
	Title     string               `json:"title"`
	Detail    string               `json:"detail"`
	Severity  string               `json:"severity"`
	Path      string               `json:"path"`
	StartLine int64                `json:"start_line"`
	EndLine   int64                `json:"end_line"`
}

// Pointers enforce the public request's required scalar presence. The domain
// request remains unchanged, including its canonical receipt fingerprint.
type decisionHTTPRequest struct {
	ExpectedRouteRevision *int64  `json:"expected_route_revision,omitempty"`
	ExpectedRevision      *int64  `json:"expected_revision"`
	Finding               *int64  `json:"finding"`
	Disposition           *string `json:"disposition"`
	Reason                *string `json:"reason"`
}

type fixHTTPRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	Finding          *int64  `json:"finding"`
	ExpectedSHA      *string `json:"expected_sha"`
	FixSHA           *string `json:"fix_sha"`
	ProofNote        *string `json:"proof_note"`
}

func publicEvidence(value ReviewEvidence) evidenceResponse {
	capture := value.Capture
	channel := evidenceChannel(capture)
	var hosted *hostedPublicationResponse
	if h := capture.Hosted; h != nil {
		hosted = &hostedPublicationResponse{PublicationID: h.PublicationID, ProviderAttemptID: h.ProviderAttemptID, TriggerCommentID: h.TriggerCommentID, ReviewIDs: append([]int64{}, h.ReviewIDs...), ObservedBaseSHA: h.ObservedBaseSHA}
		if h.StandingSummary != nil {
			standing := *h.StandingSummary
			hosted.StandingSummary = &standing
		}
		if h.SubmittedReview != nil {
			submitted := *h.SubmittedReview
			hosted.SubmittedReview = &submitted
		}
	}
	files := append([]string{}, capture.Review.ReviewedFiles...)
	findings := make([]sourceFindingResponse, len(capture.Review.Findings))
	for i, finding := range capture.Review.Findings {
		findings[i] = sourceFindingResponse{Finding: int64(i), Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity, Path: finding.Path, StartLine: finding.StartLine, EndLine: finding.EndLine}
		if capture.Hosted != nil {
			source := capture.Hosted.Sources[i]
			findings[i].Source = &source
		}
	}
	return evidenceResponse{
		ID: value.ID, Channel: channel, ProjectID: value.ProjectID, AttemptID: value.AttemptID, OperationID: value.OperationID,
		CapabilityFingerprint: value.CapabilityFingerprint, Anchor: value.Anchor, RecordedAt: value.RecordedAt,
		Capture: captureResponse{Hosted: hosted, CaptureID: capture.CaptureID, CaptureSHA256: capture.CaptureSHA256, CaptureBytes: capture.CaptureBytes,
			StartedAt: capture.StartedAt, FinishedAt: capture.FinishedAt, Outcome: capture.Review.Outcome,
			Complete: capture.Review.Complete, ReportedFindings: capture.Review.ReportedFindings, ReviewedFiles: files, Findings: findings},
	}
}

func (e *Engine) evidenceHTTPScope(w http.ResponseWriter, r *http.Request, permission string) (core.Scope, string, bool) {
	scope, err := e.store.Scope(r, permission)
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, "", false
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return core.Scope{}, "", false
	}
	id := r.PathValue("evidence_id")
	if !evidenceHTTPID.MatchString(id) {
		core.WriteError(w, invalid())
		return core.Scope{}, "", false
	}
	return scope, id, true
}

func (e *Engine) readEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "read")
	if !ok {
		return
	}
	value, err := e.ReadReviewEvidence(r.Context(), scope, id)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, publicEvidence(value))
}

func (e *Engine) readAdjudicationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "read")
	if !ok {
		return
	}
	value, err := e.ReadReviewAdjudication(r.Context(), scope, id)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) decideEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire decisionHTTPRequest
	wire.ExpectedRouteRevision = new(int64)
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.Finding == nil || wire.Disposition == nil || wire.Reason == nil || wire.ExpectedRouteRevision == nil {
		core.WriteError(w, invalid())
		return
	}
	request := FindingDecisionRequest{ExpectedRevision: wire.ExpectedRevision, Finding: *wire.Finding, Disposition: *wire.Disposition, Reason: *wire.Reason, ExpectedRouteRevision: *wire.ExpectedRouteRevision}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.DecideReviewFinding(r.Context(), scope, key, id, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}

func (e *Engine) fixEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire fixHTTPRequest
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.Finding == nil || wire.ExpectedSHA == nil || wire.FixSHA == nil || wire.ProofNote == nil {
		core.WriteError(w, invalid())
		return
	}
	request := FindingFixRequest{ExpectedRevision: wire.ExpectedRevision, Finding: *wire.Finding, ExpectedSHA: wire.ExpectedSHA, FixSHA: *wire.FixSHA, ProofNote: *wire.ProofNote}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.RecordFindingFix(r.Context(), scope, key, id, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}

func (e *Engine) finalizeEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var request FinalizeReviewRequest
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if request.ExpectedRevision == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.FinalizeReviewEvidence(r.Context(), scope, key, id, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}
