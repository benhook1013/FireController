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

func (e *Engine) evaluateFrontPlan(ctx context.Context, q *sql.Tx, r Run, repo int64, plan *SourcePlan) (evaluatedFront, error) {
	if plan.Repository.ID != repo || !sameTarget(plan.Pull, repo, plan.Pull.Number) {
		return evaluatedFront{state: "unknown", reason: "source_unavailable"}, nil
	}
	if plan.SourceReason != "" {
		return evaluatedFront{state: "held", reason: plan.SourceReason, plan: plan}, nil
	}
	if err := requireSourceObligationsClear(ctx, q, r.ProjectID, plan.Pull.Number); err != nil {
		var failure *core.Error
		if errors.As(err, &failure) {
			switch failure.Code {
			case "review_held":
				if details, ok := failure.Details.(map[string]string); ok && details["reason"] == "repository_changed" {
					return evaluatedFront{state: "held", reason: "repository_changed", plan: plan}, nil
				}
				return evaluatedFront{state: "held", reason: "source_obligations_pending", plan: plan}, nil
			case "storage_unavailable":
				return evaluatedFront{state: "held", reason: "source_obligations_unavailable", plan: plan}, nil
			}
		}
		return evaluatedFront{}, err
	}
	if plan.Pull.Merged {
		return evaluatedFront{state: "complete", plan: plan}, nil
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
	state, err := channelProgress(ctx, q, r.ProjectID, repo, plan.Pull.Number, r.Channel)
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
		return evaluatedFront{state: "complete", plan: plan}, nil
	}
	if plan.Pull.State != "open" {
		return evaluatedFront{state: "held", reason: "front_unavailable", plan: plan}, nil
	}
	return evaluatedFront{state: "ready", plan: plan}, nil
}

func (e *Engine) evaluateFront(ctx context.Context, q *sql.Tx, r Run, repo int64, prefix []SourcePlan, complete bool) (evaluatedFront, error) {
	byNumber := map[int64]*SourcePlan{}
	for i := range prefix {
		byNumber[prefix[i].Pull.Number] = &prefix[i]
	}
	var firstHeld *evaluatedFront
	for i := range prefix {
		plan := &prefix[i]
		result, err := e.evaluateFrontPlan(ctx, q, r, repo, plan)
		if err != nil {
			return evaluatedFront{}, err
		}
		if result.state == "unknown" || globalFrontHold(result.reason) {
			return result, nil
		}
		if result.state == "complete" {
			continue
		}
		if result.state == "ready" {
			seen := map[int64]bool{plan.Pull.Number: true}
			parentNumber, parentSHA := plan.ParentPullNumber, plan.ParentSHA
			for parentNumber != 0 {
				parent := byNumber[parentNumber]
				if seen[parentNumber] || parent == nil || parent.Pull.HeadSHA != parentSHA {
					result = evaluatedFront{state: "held", reason: "predecessor_unfinished", plan: plan}
					break
				}
				seen[parentNumber] = true
				parentState, err := e.evaluateFrontPlan(ctx, q, r, repo, parent)
				if err != nil {
					return evaluatedFront{}, err
				}
				if parentState.state != "complete" {
					result = evaluatedFront{state: "held", reason: "predecessor_unfinished", plan: plan}
					break
				}
				parentNumber, parentSHA = parent.ParentPullNumber, parent.ParentSHA
			}
		}
		if result.state == "ready" {
			return result, nil
		}
		if firstHeld == nil {
			saved := result
			firstHeld = &saved
		}
	}
	if firstHeld != nil {
		return *firstHeld, nil
	}
	if !complete {
		return evaluatedFront{state: "unknown", reason: "source_prefix_incomplete"}, nil
	}
	return evaluatedFront{state: "complete", reason: "queue_complete"}, nil
}

func globalFrontHold(reason string) bool {
	switch reason {
	case "repository_changed", "source_obligations_unavailable", "routed_findings_unavailable", "review_evidence_unavailable":
		return true
	}
	return false
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
