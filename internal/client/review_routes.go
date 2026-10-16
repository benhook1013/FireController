package client

import (
	"context"
	"net/http"
	"strconv"
)

// ReviewFindingRoute is receiver state attached to a stored source finding.
// Its decision and fix proof are independent of the source disposition.
type ReviewFindingRoute struct {
	ID               string `json:"id"`
	TargetPullNumber int64  `json:"target_pull_number"`
	Revision         int64  `json:"revision"`
	State            string `json:"state"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
	Actor            string `json:"actor"`
	DecidedAt        string `json:"decided_at"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
	FixedAt          string `json:"fixed_at"`
}

type ReviewRoute struct {
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	RepositoryID     int64  `json:"repository_id"`
	SourceEvidenceID string `json:"source_evidence_id"`
	Finding          int64  `json:"finding"`
	SourcePullNumber int64  `json:"source_pull_number"`
	SourceHeadSHA    string `json:"source_head_sha"`
	TargetPullNumber int64  `json:"target_pull_number"`
	State            string `json:"state"`
	Revision         int64  `json:"revision"`
	Disposition      string `json:"disposition"`
	RoutingReason    string `json:"routing_reason"`
	RoutingActorID   string `json:"routing_actor_id"`
	Reason           string `json:"reason"`
	ActorID          string `json:"actor_id"`
	DecidedAt        string `json:"decided_at"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
	FixedAt          string `json:"fixed_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type ReviewRouteDetail struct {
	Route         ReviewRoute           `json:"route"`
	SourceAnchor  ReviewAnchor          `json:"source_anchor"`
	SourceFinding ReviewEvidenceFinding `json:"source_finding"`
}

type ReviewFindingRouteSummary struct {
	ID               string `json:"id"`
	Revision         int64  `json:"revision"`
	SourceEvidenceID string `json:"source_evidence_id"`
	Finding          int64  `json:"finding"`
	SourcePullNumber int64  `json:"source_pull_number"`
	TargetPullNumber int64  `json:"target_pull_number"`
	Disposition      string `json:"disposition"`
	HasFix           bool   `json:"has_fix"`
	CreatedAt        string `json:"created_at"`
}

type ReviewRoutePage struct {
	ProjectID  string                      `json:"project_id"`
	PullNumber int64                       `json:"pull_number"`
	Items      []ReviewFindingRouteSummary `json:"items"`
	Cursor     string                      `json:"cursor,omitempty"`
	HasMore    bool                        `json:"has_more"`
}

type ReviewRouteRequest struct {
	ExpectedRevision      int64  `json:"expected_revision"`
	Finding               int64  `json:"finding"`
	TargetPullNumber      int64  `json:"target_pull_number"`
	Reason                string `json:"reason"`
	ExpectedRouteRevision int64  `json:"expected_route_revision,omitempty"`
}

type ReviewRouteReceipt struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Revision       int64  `json:"revision"`
	SourceRevision int64  `json:"source_revision"`
}

type ReviewRouteDecisionRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
}

type ReviewRouteFixRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	ExpectedSHA      string `json:"expected_sha"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
}

func ValidateReviewRouteID(id string) error {
	if !validReviewOpaqueID(id, "review_route_") {
		return validation()
	}
	return nil
}
func ValidateReviewRouteRequest(r ReviewRouteRequest) error {
	if !validControlRevision(r.ExpectedRevision) || !validControlRevision(r.ExpectedRouteRevision) || r.Finding < 0 || r.Finding >= 100 || !validPullNumber(r.TargetPullNumber) || !controlText(r.Reason, 2048, true) {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewRouteDecisionRequest(r ReviewRouteDecisionRequest) error {
	if r.ExpectedRevision < 1 || !validControlRevision(r.ExpectedRevision) || !oneReviewOf(r.Disposition, "accepted", "rejected") || !controlText(r.Reason, 2048, true) {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewRouteFixRequest(r ReviewRouteFixRequest) error {
	if r.ExpectedRevision < 1 || !validControlRevision(r.ExpectedRevision) || r.ExpectedSHA != "" && !controlHex(r.ExpectedSHA, 40) || !controlHex(r.FixSHA, 40) || !controlText(r.ProofNote, 2048, true) {
		return mutationValidation()
	}
	return nil
}

func (c *Client) CreateReviewFindingRoute(ctx context.Context, key, evidenceID string, r ReviewRouteRequest) (ReviewRouteReceipt, error) {
	if !validMutationKey(key) || ValidateReviewEvidenceID(evidenceID) != nil || ValidateReviewRouteRequest(r) != nil {
		return ReviewRouteReceipt{}, mutationValidation()
	}
	var value ReviewRouteReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "evidence", evidenceID, "routes"), key, r, http.StatusOK, &value); err != nil {
		return ReviewRouteReceipt{}, err
	}
	if ValidateReviewRouteID(value.ID) != nil || value.State != "routed" || value.Revision != 1 || value.SourceRevision != r.ExpectedRevision+1 {
		return ReviewRouteReceipt{}, unknownMutation("invalid_response", "The service returned an invalid route receipt; its outcome is unknown.", http.StatusOK)
	}
	return value, nil
}

func (c *Client) ReviewFindingRoute(ctx context.Context, id string) (ReviewRouteDetail, error) {
	if ValidateReviewRouteID(id) != nil {
		return ReviewRouteDetail{}, validation()
	}
	var value ReviewRouteDetail
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "routes", id), nil, true, "", nil, http.StatusOK, false, &value, 256<<10, decodeReviewResponse); err != nil {
		return ReviewRouteDetail{}, err
	}
	r, a, f := value.Route, value.SourceAnchor, value.SourceFinding
	if r.ID != id || r.ProjectID != c.identity.ProjectID || r.RepositoryID <= 0 || ValidateReviewEvidenceID(r.SourceEvidenceID) != nil || r.Finding < 0 || r.Finding >= 100 || !validPullNumber(r.SourcePullNumber) || r.SourcePullNumber == r.TargetPullNumber || !controlHex(r.SourceHeadSHA, 40) || !controlText(r.RoutingReason, 2048, true) || !validID(r.RoutingActorID) || !controlTime(r.CreatedAt) || !controlTime(r.UpdatedAt) || !validControlAnchor(a) || a.RepositoryID != r.RepositoryID || a.PullNumber != r.SourcePullNumber || a.CandidateSHA != r.SourceHeadSHA || f.Finding != r.Finding || !validRouteSourceFinding(f) || !validFindingRoute(ReviewFindingRoute{ID: r.ID, TargetPullNumber: r.TargetPullNumber, Revision: r.Revision, State: r.State, Disposition: r.Disposition, Reason: r.Reason, Actor: r.ActorID, DecidedAt: r.DecidedAt, FixSHA: r.FixSHA, ProofNote: r.ProofNote, FixedAt: r.FixedAt}) {
		return ReviewRouteDetail{}, invalidResponse()
	}
	return value, nil
}

func validRouteSourceFinding(f ReviewEvidenceFinding) bool {
	return controlText(f.Title, 500, true) && controlText(f.Detail, 16<<10, false) && oneReviewOf(f.Severity, "info", "low", "medium", "high", "critical", "unknown") && (f.Path == "" || controlPath(f.Path)) && f.StartLine >= 0 && f.EndLine >= 0 && (f.StartLine == 0 || f.EndLine == 0 || f.EndLine >= f.StartLine)
}
func validFindingRoute(r ReviewFindingRoute) bool {
	if ValidateReviewRouteID(r.ID) != nil || !validPullNumber(r.TargetPullNumber) || r.Revision < 1 || !oneReviewOf(r.State, "active", "superseded") || !oneReviewOf(r.Disposition, "unadjudicated", "accepted", "rejected") || !controlText(r.Reason, 2048, false) || !controlText(r.ProofNote, 2048, false) {
		return false
	}
	if r.Disposition == "unadjudicated" {
		return r.Reason == "" && r.Actor == "" && r.DecidedAt == "" && r.FixSHA == "" && r.ProofNote == "" && r.FixedAt == ""
	}
	if !controlText(r.Reason, 2048, true) || !validID(r.Actor) || !controlTime(r.DecidedAt) {
		return false
	}
	if r.FixSHA == "" {
		return r.ProofNote == "" && r.FixedAt == ""
	}
	return r.State == "active" && r.Disposition == "accepted" && controlHex(r.FixSHA, 40) && controlText(r.ProofNote, 2048, true) && controlTime(r.FixedAt)
}

func (c *Client) ReviewFindingRoutes(ctx context.Context, number int64, options PageOptions) (ReviewRoutePage, error) {
	if options.Limit == 0 {
		options.Limit = 5
	}
	query, err := reviewPageQuery(options)
	if err != nil || !validPullNumber(number) {
		return ReviewRoutePage{}, validation()
	}
	query.Set("pull_number", strconv.FormatInt(number, 10))
	var value ReviewRoutePage
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "routes"), query, true, "", nil, http.StatusOK, false, &value, maxReviewListResponse, decodeReviewResponse); err != nil {
		return ReviewRoutePage{}, err
	}
	if value.ProjectID != c.identity.ProjectID || value.PullNumber != number || !validReviewPage(value.Items, value.Cursor, value.HasMore, options) {
		return ReviewRoutePage{}, invalidResponse()
	}
	seen := map[string]bool{}
	for _, r := range value.Items {
		if ValidateReviewRouteID(r.ID) != nil || seen[r.ID] || r.Revision < 1 || ValidateReviewEvidenceID(r.SourceEvidenceID) != nil || r.Finding < 0 || r.Finding >= 100 || !validPullNumber(r.SourcePullNumber) || r.SourcePullNumber == number || r.TargetPullNumber != number || !oneReviewOf(r.Disposition, "unadjudicated", "accepted", "rejected") || r.HasFix && r.Disposition != "accepted" || !controlTime(r.CreatedAt) {
			return ReviewRoutePage{}, invalidResponse()
		}
		seen[r.ID] = true
	}
	return value, nil
}

func (c *Client) DecideReviewFindingRoute(ctx context.Context, key, id string, r ReviewRouteDecisionRequest) (ReviewControlReceipt, error) {
	if ValidateReviewRouteDecisionRequest(r) != nil {
		return ReviewControlReceipt{}, mutationValidation()
	}
	return c.reviewRouteControl(ctx, key, id, "decisions", "adjudicated", r.ExpectedRevision, r)
}
func (c *Client) FixReviewFindingRoute(ctx context.Context, key, id string, r ReviewRouteFixRequest) (ReviewControlReceipt, error) {
	if ValidateReviewRouteFixRequest(r) != nil {
		return ReviewControlReceipt{}, mutationValidation()
	}
	return c.reviewRouteControl(ctx, key, id, "fixes", "fix_recorded", r.ExpectedRevision, r)
}
func (c *Client) reviewRouteControl(ctx context.Context, key, id, suffix, state string, expected int64, r any) (ReviewControlReceipt, error) {
	if !validMutationKey(key) || ValidateReviewRouteID(id) != nil {
		return ReviewControlReceipt{}, mutationValidation()
	}
	var value ReviewControlReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "routes", id, suffix), key, r, http.StatusOK, &value); err != nil {
		return ReviewControlReceipt{}, err
	}
	if value.ID != id || value.State != state || value.Revision != expected+1 {
		return ReviewControlReceipt{}, unknownMutation("invalid_response", "The service returned an invalid route receipt; its outcome is unknown.", http.StatusOK)
	}
	return value, nil
}
