package reviewexec

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// WithNativeProjection connects journal transitions and adjudication changes
// to the native browser projection in the same transaction. The owner must
// install the reviews and evidence schemas before accepting requests. Isolated journal users
// need not install a browser projection.
func WithNativeProjection() EngineOption {
	return func(e *Engine) {
		e.publishAdjudication = publishReviewAdjudication
		e.publishAttempt = publishAttemptTransition
	}
}

func projectionFindingID(evidenceID string, finding int64) string {
	return evidenceID + ":finding:" + strconv.FormatInt(finding, 10)
}

func publishReviewAdjudication(ctx context.Context, tx *sql.Tx, scope core.Scope, state ReviewAdjudication) error {
	input := reviews.NativeAdjudication{
		SourceRunID: state.EvidenceID,
		Finalized:   state.Finalized,
		Findings:    make([]reviews.NativeFindingUpdate, 0, len(state.Findings)),
	}
	if state.FrozenCounts != nil {
		input.FrozenCounts = &reviews.NativeCounts{
			Found:    state.FrozenCounts.Found,
			Accepted: state.FrozenCounts.Accepted,
			Routed:   state.FrozenCounts.Routed,
		}
	}
	var evidence ReviewEvidence
	for _, finding := range state.Findings {
		if finding.RouteID != "" {
			var err error
			evidence, _, err = loadReviewAdjudication(ctx, tx, scope.ProjectID, state.EvidenceID)
			if err != nil {
				return err
			}
			break
		}
	}
	for _, finding := range state.Findings {
		route, err := projectedFindingRoute(ctx, tx, evidence, finding)
		if err != nil {
			return err
		}
		input.Findings = append(input.Findings, reviews.NativeFindingUpdate{
			Route:           route,
			SourceFindingID: projectionFindingID(state.EvidenceID, finding.Finding),
			Disposition:     finding.Disposition,
			DecisionReason:  finding.Reason,
			Actor:           finding.ActorID,
			DecidedAt:       finding.DecidedAt,
			FixSHA:          finding.FixSHA,
			ProofNote:       finding.ProofNote,
			ResolvedAt:      finding.FixedAt,
		})
	}
	_, _, err := reviews.UpdateNativeAdjudication(ctx, tx, scope, input)
	return err
}

// projectedFindingRoute derives receiving state from the authoritative ledger
// inside the caller's transaction. Imported display rows cannot supply proof.
func projectedFindingRoute(ctx context.Context, tx EvidenceQuery, evidence ReviewEvidence, finding FindingDecision) (*reviews.FindingRoute, error) {
	if finding.Disposition != "routed" {
		if finding.RouteID != "" {
			return nil, adjudicationStorageError()
		}
		return nil, nil
	}
	if finding.RouteID == "" || finding.FixSHA != "" || finding.ProofNote != "" || finding.FixedAt != "" {
		return nil, adjudicationStorageError()
	}
	route, err := readFindingRoute(ctx, tx, evidence.ProjectID, finding.RouteID)
	if err != nil {
		return nil, err
	}
	if route.ID != finding.RouteID || route.ProjectID != evidence.ProjectID || route.RepositoryID != evidence.Anchor.RepositoryID || route.SourceEvidenceID != evidence.ID || route.Finding != finding.Finding || route.SourcePullNumber != evidence.Anchor.PullNumber || route.SourceHeadSHA != evidence.Anchor.CandidateSHA || route.TargetPullNumber == route.SourcePullNumber || route.State != "active" || route.RoutingReason != finding.Reason || route.RoutingActorID != finding.ActorID || route.CreatedAt != finding.DecidedAt {
		return nil, adjudicationStorageError()
	}
	return &reviews.FindingRoute{ID: route.ID, TargetPullNumber: route.TargetPullNumber, Revision: route.Revision, State: route.State, Disposition: route.Disposition, Reason: route.Reason, Actor: route.ActorID, DecidedAt: route.DecidedAt, FixSHA: route.FixSHA, ProofNote: route.ProofNote, FixedAt: route.FixedAt}, nil
}

func attachFindingRoutes(ctx context.Context, tx *sql.Tx, evidence ReviewEvidence, state ReviewAdjudication, run *reviews.RunInput) error {
	if len(run.Findings) != len(state.Findings) {
		return adjudicationStorageError()
	}
	for i, finding := range state.Findings {
		route, err := projectedFindingRoute(ctx, tx, evidence, finding)
		if err != nil {
			return err
		}
		run.Findings[i].Route = route
	}
	return nil
}

func receivingRouteResolved(route *reviews.FindingRoute) (bool, error) {
	if route == nil || !safeText(route.Reason, 2048) || !safeText(route.ProofNote, 2048) {
		return false, adjudicationStorageError()
	}
	if route.Disposition == "unadjudicated" {
		if route.Reason != "" || route.Actor != "" || route.DecidedAt != "" || route.FixSHA != "" || route.ProofNote != "" || route.FixedAt != "" {
			return false, adjudicationStorageError()
		}
		return false, nil
	}
	decided, err := time.Parse(time.RFC3339Nano, route.DecidedAt)
	if strings.TrimSpace(route.Reason) == "" || !identifier.MatchString(route.Actor) || err != nil || decided.IsZero() {
		return false, adjudicationStorageError()
	}
	if route.FixSHA == "" {
		if route.ProofNote != "" || route.FixedAt != "" {
			return false, adjudicationStorageError()
		}
		if route.Disposition == "accepted" {
			return false, nil
		}
		if route.Disposition == "rejected" {
			return true, nil
		}
		return false, adjudicationStorageError()
	}
	fixed, err := time.Parse(time.RFC3339Nano, route.FixedAt)
	if route.Disposition != "accepted" || !sha.MatchString(route.FixSHA) || strings.TrimSpace(route.ProofNote) == "" || err != nil || fixed.IsZero() {
		return false, adjudicationStorageError()
	}
	return true, nil
}
