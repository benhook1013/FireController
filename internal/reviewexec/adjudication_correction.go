package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

type FindingFixCorrectionRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	ExpectedSHA      string `json:"expected_sha"`
	Finding          *int64 `json:"finding"`
	Reason           string `json:"reason"`
}

// CorrectFinalizedFindingFix rejects one formerly accepted finding while
// retaining its original decision and commit proof in the private audit.
func (e *Engine) CorrectFinalizedFindingFix(ctx context.Context, scope core.Scope, key, evidenceID string, request FindingFixCorrectionRequest) (json.RawMessage, error) {
	if !evidenceHTTPID.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) ||
		request.Finding == nil || *request.Finding < 0 || *request.Finding >= 100 ||
		!sha.MatchString(request.ExpectedSHA) || !safeText(request.Reason, 2048) || strings.TrimSpace(request.Reason) == "" {
		return nil, invalid()
	}
	expected, index := *request.ExpectedRevision, *request.Finding
	request.ExpectedRevision, request.Finding = &expected, &index
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.correct_fix", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, adjudicationRevisionConflict(state.Revision)
		}
		if index >= int64(len(state.Findings)) {
			return nil, invalid()
		}
		before := state.Findings[index]
		if !state.Finalized || before.Disposition != "accepted" || before.RouteID != "" || before.OperationalProof != nil {
			return nil, conflict()
		}
		if before.FixSHA != request.ExpectedSHA {
			return nil, findingFixGuardConflict(state.Revision, before, "fix_sha_conflict")
		}
		if _, ok := attemptRecordedTime(before.FixedAt); !ok || strings.TrimSpace(before.ProofNote) == "" {
			return nil, adjudicationStorageError()
		}
		at := stamp()
		state.Findings[index] = FindingDecision{Finding: index, Disposition: "rejected", Reason: request.Reason, ActorID: scope.PrincipalID, DecidedAt: at}
		correction := &reviews.NativeFixCorrection{
			SourceFindingID: projectionFindingID(evidenceID, index),
			PreviousSHA:     before.FixSHA, PreviousNote: before.ProofNote, PreviousResolvedAt: before.FixedAt,
		}
		audit := struct {
			FindingFixCorrectionRequest
			Previous FindingDecision `json:"previous"`
		}{request, before}
		return e.persistReviewAdjudicationChange(ctx, tx, scope, state, "adjudication_corrected", index, audit, at, correction)
	})
}
