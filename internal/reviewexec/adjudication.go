package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type FindingDecisionRequest struct {
	ExpectedRevision      *int64 `json:"expected_revision"`
	Finding               int64  `json:"finding"`
	Disposition           string `json:"disposition"`
	Reason                string `json:"reason"`
	ExpectedRouteRevision int64  `json:"expected_route_revision,omitempty"`
}

type FinalizeReviewRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
}

type FindingFixRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	Finding          int64   `json:"finding"`
	ExpectedSHA      *string `json:"expected_sha"`
	FixSHA           string  `json:"fix_sha"`
	ProofNote        string  `json:"proof_note"`
}

type FindingCounts struct {
	Found    int64 `json:"found"`
	Accepted int64 `json:"accepted"`
	Routed   int64 `json:"routed"`
}

type FindingDecision struct {
	OperationalProof *reviews.OperationalProof `json:"operational_proof,omitempty"`
	Finding          int64                     `json:"finding"`
	Disposition      string                    `json:"disposition"`
	Reason           string                    `json:"reason"`
	ActorID          string                    `json:"actor_id"`
	DecidedAt        string                    `json:"decided_at"`
	FixSHA           string                    `json:"fix_sha"`
	ProofNote        string                    `json:"proof_note"`
	FixedAt          string                    `json:"fixed_at"`
	RouteID          string                    `json:"route_id,omitempty"`
}

// ReviewAdjudication contains dispositions and proof references, not finding text.
// FrozenCounts remain the original finalized source counts after corrections.
type ReviewAdjudication struct {
	EvidenceID             string                                     `json:"evidence_id"`
	Revision               int64                                      `json:"revision"`
	Finalized              bool                                       `json:"finalized"`
	FrozenCounts           *FindingCounts                             `json:"frozen_counts"`
	FrozenAcceptedSeverity *reviewprovider.CheckpointAcceptedSeverity `json:"frozen_accepted_severity,omitempty"`
	Findings               []FindingDecision                          `json:"findings"`
}

func adjudicationStorageError() error {
	return core.Fail("storage_unavailable", "Stored review adjudication is unavailable.", 500)
}

// loadReviewAdjudication reads the immutable source and current projection in the
// same snapshot. Source text is used internally and never included in the view.
func loadReviewAdjudication(ctx context.Context, tx EvidenceQuery, project, id string) (ReviewEvidence, ReviewAdjudication, error) {
	var captureBody string
	var stateBody sql.NullString
	var revision sql.NullInt64
	var evidence ReviewEvidence
	var state ReviewAdjudication
	err := tx.QueryRowContext(ctx, `SELECT e.body_json,s.revision,s.body_json FROM review_execution_evidence e LEFT JOIN review_adjudication_state s ON s.evidence_id=e.id AND s.project_id=e.project_id WHERE e.project_id=? AND e.id=?`, project, id).Scan(&captureBody, &revision, &stateBody)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence, state, core.Fail("not_found", "The review capture is unavailable in this project.", 404)
	}
	if err != nil {
		return evidence, state, err
	}
	if len(captureBody) > evidenceInputLimit+8192 || json.Unmarshal([]byte(captureBody), &evidence) != nil || evidence.ID != id || evidence.ProjectID != project || validateReviewEvidence(evidence.Capture) != nil {
		return evidence, state, adjudicationStorageError()
	}
	if !revision.Valid {
		state = ReviewAdjudication{EvidenceID: id, Findings: make([]FindingDecision, len(evidence.Capture.Review.Findings))}
		for i := range state.Findings {
			state.Findings[i] = FindingDecision{Finding: int64(i), Disposition: "unadjudicated"}
		}
		return evidence, state, nil
	}
	// At most 100 entries, each with two 2048-byte text fields; JSON escaping
	// can expand each byte sixfold. This also bounds corrupted stored records.
	if !stateBody.Valid || len(stateBody.String) > 3<<20 || json.Unmarshal([]byte(stateBody.String), &state) != nil || state.EvidenceID != id || state.Revision != revision.Int64 || state.Revision < 1 || len(state.Findings) != len(evidence.Capture.Review.Findings) || state.Finalized != (state.FrozenCounts != nil) {
		return evidence, state, adjudicationStorageError()
	}
	if state.Findings == nil {
		state.Findings = []FindingDecision{}
	}
	for i, finding := range state.Findings {
		if !validOperationalDecision(finding) {
			return evidence, state, adjudicationStorageError()
		}
		if finding.Finding != int64(i) || finding.Disposition != "unadjudicated" && finding.Disposition != "accepted" && finding.Disposition != "rejected" && finding.Disposition != "routed" || !safeText(finding.Reason, 2048) || !safeText(finding.ProofNote, 2048) {
			return evidence, state, adjudicationStorageError()
		}
		if finding.Disposition == "routed" {
			if !validFindingRouteID(finding.RouteID) || finding.FixSHA != "" || finding.ProofNote != "" || finding.FixedAt != "" {
				return evidence, state, adjudicationStorageError()
			}
		} else if finding.RouteID != "" {
			return evidence, state, adjudicationStorageError()
		}
	}
	if state.FrozenAcceptedSeverity != nil && (state.FrozenCounts == nil || !state.FrozenAcceptedSeverity.Valid(state.FrozenCounts.Accepted)) {
		return evidence, state, adjudicationStorageError()
	}
	if state.FrozenCounts != nil {
		c := state.FrozenCounts
		if c.Found != int64(len(state.Findings)) || c.Accepted < 0 || c.Routed < 0 || c.Accepted > c.Found || c.Routed > c.Found-c.Accepted {
			return evidence, state, adjudicationStorageError()
		}
	}
	return evidence, state, nil
}

func (e *Engine) ReadReviewAdjudication(ctx context.Context, scope core.Scope, evidenceID string) (ReviewAdjudication, error) {
	if !identifier.MatchString(evidenceID) {
		return ReviewAdjudication{}, invalid()
	}
	var state ReviewAdjudication
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		_, state, err = loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		return err
	})
	if err != nil {
		return ReviewAdjudication{}, err
	}
	return state, nil
}

func validAdjudicationRevision(expected *int64) bool { return expected != nil && *expected >= 0 }

func adjudicationMatches(state ReviewAdjudication, expected int64) bool {
	return state.Revision == expected && state.Revision < math.MaxInt64
}

// persistReviewAdjudication atomically changes the disposition view, audit and
// event, plus an installed native browser projection. Mutate creates the receipt;
// projection failure rolls back all changes without altering captured evidence.
func (e *Engine) persistReviewAdjudication(ctx context.Context, tx *sql.Tx, scope core.Scope, state ReviewAdjudication, kind string, finding int64, request any, at string) (any, error) {
	return e.persistReviewAdjudicationChange(ctx, tx, scope, state, kind, finding, request, at, nil)
}

func (e *Engine) persistReviewAdjudicationChange(ctx context.Context, tx *sql.Tx, scope core.Scope, state ReviewAdjudication, kind string, finding int64, request any, at string, correction *reviews.NativeFixCorrection) (any, error) {
	prior := state.Revision
	state.Revision++
	body, err := json.Marshal(state)
	if err != nil {
		return nil, adjudicationStorageError()
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO review_adjudication_state(evidence_id,project_id,revision,body_json) VALUES(?,?,?,?) ON CONFLICT(evidence_id) DO UPDATE SET revision=excluded.revision,body_json=excluded.body_json WHERE review_adjudication_state.project_id=excluded.project_id AND review_adjudication_state.revision=?`, state.EvidenceID, scope.ProjectID, state.Revision, string(body), prior)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, conflict()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_adjudication_changes(evidence_id,revision,project_id,actor_id,kind,finding_index,input_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, state.EvidenceID, state.Revision, scope.ProjectID, scope.PrincipalID, kind, finding, encoded(request), at); err != nil {
		return nil, err
	}
	eventKind := kind
	if eventKind == "operational_fix_recorded" {
		eventKind = "fix_recorded"
	}
	if correction != nil {
		eventKind = "adjudicated"
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.evidence."+eventKind, state.EvidenceID, state.Revision, struct {
		Finding int64 `json:"finding"`
	}{finding}); err != nil {
		return nil, err
	}
	if correction != nil && e.publishAdjudication != nil {
		if err = publishReviewAdjudicationWithCorrection(ctx, tx, scope, state, correction); err != nil {
			return nil, err
		}
	} else if e.publishAdjudication != nil {
		if err = e.publishAdjudication(ctx, tx, scope, state); err != nil {
			return nil, err
		}
		evidence, _, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, state.EvidenceID)
		if err != nil {
			return nil, err
		}
		if err := e.followExplicitWork(ctx, tx, scope, evidence.Anchor.PullNumber); err != nil {
			return nil, err
		}
	}
	if err = refreshEvidenceComparison(ctx, tx, scope.ProjectID, state.EvidenceID); err != nil {
		return nil, err
	}
	if state.Finalized && correction == nil {
		var operation string
		if err = tx.QueryRowContext(ctx, `SELECT operation_id FROM review_execution_evidence WHERE project_id=? AND id=?`, scope.ProjectID, state.EvidenceID).Scan(&operation); err != nil {
			return nil, err
		}
		if err = completeRoundControl(ctx, tx, scope.ProjectID, operation); err != nil {
			return nil, err
		}
	}
	return Receipt{ID: state.EvidenceID, State: kind, Revision: state.Revision}, nil
}

func (e *Engine) DecideReviewFinding(ctx context.Context, scope core.Scope, key, evidenceID string, request FindingDecisionRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) || !validFindingDecisionAction(request) {
		return nil, invalid()
	}
	// Copy pointer values so the callback and receipt fingerprint use one input.
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	queued := false
	raw, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.decide", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		evidence, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, adjudicationRevisionConflict(state.Revision)
		}
		wasAwaitingDecision := hasUndecidedFindings(state)
		at, err := e.applyFindingDecision(ctx, tx, scope, evidence, &state, request)
		if err != nil {
			return nil, err
		}
		return e.persistFindingDecision(ctx, tx, scope, evidence, state, wasAwaitingDecision, "adjudicated", request.Finding, request, at, &queued)
	})
	if err == nil && queued {
		e.wakeCheckpoints()
	}
	return raw, err
}

func hasUndecidedFindings(state ReviewAdjudication) bool {
	return !state.Finalized && slices.ContainsFunc(state.Findings, func(finding FindingDecision) bool {
		return finding.Disposition == "unadjudicated"
	})
}

// A decision can finish a newly settled source. An ordinary correction on a
// historical all-decided record leaves its explicit finalization path intact.
func (e *Engine) persistFindingDecision(ctx context.Context, tx *sql.Tx, scope core.Scope, evidence ReviewEvidence, state ReviewAdjudication, wasAwaitingDecision bool, kind string, finding int64, request any, at string, queued *bool) (any, error) {
	finalize := false
	if wasAwaitingDecision && !hasUndecidedFindings(state) && (evidence.Capture.Review.Complete || usablePartialCapture(evidence.Capture)) {
		attempt, err := readAttempt(ctx, tx, evidence.ProjectID, evidence.AttemptID)
		if err != nil {
			return nil, err
		}
		finalize, err = finalizableEvidence(ctx, tx, evidence, attempt)
		if err != nil {
			return nil, err
		}
		if finalize {
			if err = freezeReviewCounts(ctx, tx, evidence, &state); err != nil {
				return nil, err
			}
		}
	}
	receipt, err := e.persistReviewAdjudication(ctx, tx, scope, state, kind, finding, request, at)
	if err != nil || !finalize {
		return receipt, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.evidence.finalized", state.EvidenceID, state.Revision+1, struct {
		Finding int64 `json:"finding"`
	}{-1}); err != nil {
		return nil, err
	}
	if _, err = e.enqueueCheckpoint(ctx, tx, scope, evidence.ID, nil); err != nil {
		return nil, err
	}
	*queued = true
	return receipt, nil
}

// Capture can precede attempt settlement. This covers empty captures and
// decisions that arrived before the completed attempt was committed.
func (e *Engine) finalizeSettledAdjudication(ctx context.Context, tx *sql.Tx, operation Operation, attempt Attempt, at string) (bool, error) {
	facts, err := operationStatusEvidence(ctx, tx, operation)
	if err != nil || !facts.Complete && !usablePartialOperation(operation) {
		return false, err
	}
	evidence, state, err := loadReviewAdjudication(ctx, tx, operation.ProjectID, facts.ID)
	if err != nil {
		return false, err
	}
	if evidence.AttemptID != attempt.ID || evidence.OperationID != operation.ID {
		return false, adjudicationStorageError()
	}
	eligible, err := finalizableEvidence(ctx, tx, evidence, attempt)
	if err != nil {
		return false, err
	}
	if !eligible && usablePartialOperation(operation) {
		return false, adjudicationStorageError()
	}
	if !eligible || state.Finalized || hasUndecidedFindings(state) {
		return false, nil
	}
	if err = freezeReviewCounts(ctx, tx, evidence, &state); err != nil {
		return false, err
	}
	scope := core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}
	if _, err = e.persistReviewAdjudication(ctx, tx, scope, state, "finalized", -1, struct{}{}, at); err != nil {
		return false, err
	}
	_, err = e.enqueueCheckpoint(ctx, tx, scope, evidence.ID, nil)
	return err == nil, err
}

func freezeReviewCounts(ctx context.Context, tx *sql.Tx, evidence ReviewEvidence, state *ReviewAdjudication) error {
	counts := FindingCounts{Found: int64(len(state.Findings))}
	for _, finding := range state.Findings {
		switch finding.Disposition {
		case "accepted":
			counts.Accepted++
		case "routed":
			route, err := readFindingRoute(ctx, tx, evidence.ProjectID, finding.RouteID)
			if err != nil {
				return err
			}
			if !routeMatchesSource(route, evidence, *state, true) {
				return routeStorageError()
			}
			counts.Routed++
		case "rejected":
		default:
			return conflict()
		}
	}
	state.Finalized, state.FrozenCounts = true, &counts
	state.FrozenAcceptedSeverity = acceptedSourceSeverity(evidence, *state)
	return nil
}

func (e *Engine) FinalizeReviewEvidence(ctx context.Context, scope core.Scope, key, evidenceID string, request FinalizeReviewRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	queued := false
	raw, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.finalize", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		evidence, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, adjudicationRevisionConflict(state.Revision)
		}
		if state.Finalized {
			return Receipt{ID: state.EvidenceID, State: "finalized", Revision: state.Revision}, nil
		}
		if !evidence.Capture.Review.Complete && !usablePartialCapture(evidence.Capture) {
			return nil, conflict()
		}
		a, err := readAttempt(ctx, tx, scope.ProjectID, evidence.AttemptID)
		if err != nil {
			return nil, err
		}
		eligible, err := finalizableEvidence(ctx, tx, evidence, a)
		if err != nil {
			return nil, err
		}
		if !eligible {
			if a.State == "uncertain" && evidence.Capture.Review.Complete {
				return nil, core.Fail("review_result_unsettled", "The complete captured result awaits settlement of its original review attempt.", 409)
			}
			return nil, conflict()
		}
		if err = freezeReviewCounts(ctx, tx, evidence, &state); err != nil {
			return nil, err
		}
		receipt, err := e.persistReviewAdjudication(ctx, tx, scope, state, "finalized", -1, request, stamp())
		if err != nil {
			return nil, err
		}
		// Persist delivery with the finalized counts. No provider call or fix
		// prerequisite belongs on the agent's finalization path.
		if _, err = e.enqueueCheckpoint(ctx, tx, scope, evidenceID, nil); err != nil {
			return nil, err
		}
		queued = true
		return receipt, nil
	})
	if err == nil && queued {
		e.wakeCheckpoints()
	}
	return raw, err
}

func (e *Engine) RecordFindingFix(ctx context.Context, scope core.Scope, key, evidenceID string, request FindingFixRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) || !validFindingFixAction(request) {
		return nil, invalid()
	}
	expected, expectedSHA := *request.ExpectedRevision, *request.ExpectedSHA
	request.ExpectedRevision, request.ExpectedSHA = &expected, &expectedSHA
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.fix", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, adjudicationRevisionConflict(state.Revision)
		}
		at, err := applyFindingFix(&state, request)
		if err != nil {
			return nil, err
		}
		return e.persistReviewAdjudication(ctx, tx, scope, state, "fix_recorded", request.Finding, request, at)
	})
}

func (e *Engine) applyFindingDecision(ctx context.Context, tx *sql.Tx, scope core.Scope, evidence ReviewEvidence, state *ReviewAdjudication, request FindingDecisionRequest) (string, error) {
	var err error
	if request.Finding >= int64(len(state.Findings)) {
		return "", invalid()
	}
	f := &state.Findings[request.Finding]
	if request.Disposition == "rejected" && (f.FixSHA != "" || f.OperationalProof != nil) {
		return "", conflict()
	}
	var at string
	if f.Disposition == "routed" {
		at = e.routeStamp("")
	} else {
		at = sourceStamp()
	}
	if f.Disposition == "routed" {
		if err = routeRepository(ctx, tx, scope.ProjectID, evidence.Anchor.RepositoryID); err != nil {
			return "", err
		}
		if at, err = supersedeFindingRoute(ctx, tx, scope, evidence, *state, *f, request.ExpectedRouteRevision, at); err != nil {
			return "", err
		}
		f.RouteID = ""
	} else if request.ExpectedRouteRevision != 0 {
		return "", conflict()
	}
	f.Disposition, f.Reason, f.ActorID, f.DecidedAt = request.Disposition, request.Reason, scope.PrincipalID, at
	return at, nil
}

func applyFindingFix(state *ReviewAdjudication, request FindingFixRequest) (string, error) {
	if request.Finding >= int64(len(state.Findings)) {
		return "", invalid()
	}
	f := &state.Findings[request.Finding]
	if f.Disposition != "accepted" {
		return "", findingFixGuardConflict(state.Revision, *f, "finding_not_accepted")
	}
	if f.OperationalProof != nil {
		return "", conflict()
	}
	if f.FixSHA != *request.ExpectedSHA {
		return "", findingFixGuardConflict(state.Revision, *f, "fix_sha_conflict")
	}
	at := stamp()
	f.FixSHA, f.ProofNote, f.FixedAt = request.FixSHA, request.ProofNote, at
	return at, nil
}

func validFindingDecisionAction(request FindingDecisionRequest) bool {
	return !(request.Finding < 0 || request.Finding >= 100 || request.ExpectedRouteRevision < 0 || request.ExpectedRouteRevision == math.MaxInt64 || request.Disposition != "accepted" && request.Disposition != "rejected" || !safeText(request.Reason, 2048) || strings.TrimSpace(request.Reason) == "")
}

func validFindingFixAction(request FindingFixRequest) bool {
	return !(request.Finding < 0 || request.Finding >= 100 || request.ExpectedSHA == nil || *request.ExpectedSHA != "" && !sha.MatchString(*request.ExpectedSHA) || !sha.MatchString(request.FixSHA) || !safeText(request.ProofNote, 2048) || strings.TrimSpace(request.ProofNote) == "")
}

// acceptedSourceSeverity runs once at source finalization. Later corrections and
// fix records preserve this original partition alongside the frozen totals.
func acceptedSourceSeverity(evidence ReviewEvidence, state ReviewAdjudication) *reviewprovider.CheckpointAcceptedSeverity {
	result := &reviewprovider.CheckpointAcceptedSeverity{}
	for i, decision := range state.Findings {
		if decision.Disposition != "accepted" {
			continue
		}
		finding := evidence.Capture.Review.Findings[i]
		severity := finding.Severity
		if (severity == "" || severity == "unknown") && evidence.Capture.Hosted != nil {
			severity = reviewprovider.HostedDisplaySeverity(finding.Detail)
		}
		switch severity {
		case "high", "major":
			result.Major++
		case "low", "minor":
			result.Minor++
		case "info", "trivial":
			result.Trivial++
		case "critical":
			result.Critical++
		default:
			result.Unknown++
		}
	}
	return result
}
