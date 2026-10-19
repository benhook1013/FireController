package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
)

// evaluatedFront is shared with admission. A diagnostic hold retains the actual
// blocking predecessor, whereas the legacy adapter intentionally returns zero.
type evaluatedFront struct {
	state, reason string
	plan          *SourcePlan
}

func (e *Engine) evaluateFront(ctx context.Context, q *sql.Tx, r Run, repo int64, prefix []SourcePlan, complete bool) (evaluatedFront, error) {
	for i := range prefix {
		plan := &prefix[i]
		if plan.Repository.ID != repo || !sameTarget(plan.Pull, repo, plan.Pull.Number) {
			return evaluatedFront{state: "unknown", reason: "source_unavailable"}, nil
		}
		if plan.Pull.Merged {
			continue
		}
		pending, err := pendingIncomingRoutes(ctx, q, r.ProjectID, plan.Pull.Number)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "storage_unavailable" {
				return evaluatedFront{state: "held", reason: "routed_findings_unavailable", plan: plan}, nil
			}
			return evaluatedFront{}, err
		}
		if pending > 0 {
			return evaluatedFront{state: "held", reason: "routed_findings_pending", plan: plan}, nil
		}
		anchor := Anchor{RepositoryID: repo, PullNumber: plan.Pull.Number, CandidateSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, ParentSHA: plan.ParentSHA, ParentPullNumber: plan.ParentPullNumber, BaseRef: plan.Pull.BaseRef}
		state, err := comparison(ctx, q, r.ProjectID, repo, plan.Pull.Number, anchor, r.Channel)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "storage_unavailable" {
				return evaluatedFront{state: "held", reason: "review_evidence_unavailable", plan: plan}, nil
			}
			return evaluatedFront{}, err
		}
		if state.Pending {
			return evaluatedFront{state: "held", reason: "review_evidence_pending", plan: plan}, nil
		}
		if state.RoundsConfigured && state.RoundsComplete || !state.RoundsConfigured && state.Covered {
			continue
		}
		if plan.Pull.State != "open" {
			return evaluatedFront{state: "held", reason: "front_unavailable", plan: plan}, nil
		}
		return evaluatedFront{state: "ready", plan: plan}, nil
	}
	if !complete {
		return evaluatedFront{state: "unknown", reason: "source_prefix_incomplete"}, nil
	}
	return evaluatedFront{state: "complete", reason: "queue_complete"}, nil
}

// runFront preserves existing admission semantics and its zero-on-hold result.
func (e *Engine) runFront(ctx context.Context, q *sql.Tx, r Run, repo int64, prefix []SourcePlan) (int64, string, error) {
	result, err := e.evaluateFront(ctx, q, r, repo, prefix, true)
	if err != nil {
		return 0, "", err
	}
	if result.state == "ready" {
		return result.plan.Pull.Number, "", nil
	}
	if result.state == "complete" {
		return 0, "", nil
	}
	return 0, result.reason, nil
}
