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
	state, reason  string
	plan           *SourcePlan
	blockingPull   int64
	blockingReason string
}

func (f evaluatedFront) holdsPull(number int64) bool {
	return number != 0 && f.state == "held" && f.plan != nil && f.plan.Pull.Number == number
}

func (e *Engine) evaluateFrontPlan(ctx context.Context, q *sql.Tx, r Run, repo int64, plan *SourcePlan) (evaluatedFront, error) {
	if plan.schedulingPaused {
		control, err := readPullScheduling(ctx, q, r.ProjectID, plan.Pull.Number)
		if err != nil {
			return evaluatedFront{}, err
		}
		if !control.Paused {
			return evaluatedFront{state: "unknown", reason: "source_prefix_incomplete"}, nil
		}
		return evaluatedFront{state: "held", reason: "pull_scheduling_paused"}, nil
	}
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
	if state.ExternalPending {
		return evaluatedFront{state: "held", reason: "external_hosted_pending", plan: plan}, nil
	}
	if state.Pending {
		return evaluatedFront{state: "held", reason: "review_evidence_pending", plan: plan}, nil
	}
	if state.RoundsConfigured && state.RoundsComplete || !state.RoundsConfigured && state.Covered {
		return evaluatedFront{state: "complete", plan: plan}, nil
	}
	scheduling, err := readPullScheduling(ctx, q, r.ProjectID, plan.Pull.Number)
	if err != nil {
		return evaluatedFront{}, err
	}
	if scheduling.Paused {
		return evaluatedFront{state: "held", reason: "pull_scheduling_paused", plan: plan}, nil
	}
	suppressed, err := e.automaticPullSuppressed(ctx, q, r, plan)
	if err != nil {
		return evaluatedFront{}, err
	}
	if suppressed {
		return evaluatedFront{state: "held", reason: "automatic_intent_retained", plan: plan}, nil
	}
	if plan.Pull.State != "open" {
		return evaluatedFront{state: "held", reason: "front_unavailable", plan: plan}, nil
	}
	return evaluatedFront{state: "ready", plan: plan}, nil
}

// evaluateCandidatePlan shares source-identity checks with lane diagnostics.
// Ancestor review sufficiency is not permission to review a child's pinned diff.
func (e *Engine) evaluateCandidatePlan(ctx context.Context, q *sql.Tx, r Run, repo int64, plan *SourcePlan, byNumber map[int64]*SourcePlan) (evaluatedFront, error) {
	return evaluateCandidateLocalState(plan, byNumber, func(p *SourcePlan) (evaluatedFront, error) { return e.evaluateFrontPlan(ctx, q, r, repo, p) })
}

func evaluateCandidateLocalState(plan *SourcePlan, byNumber map[int64]*SourcePlan, evaluate func(*SourcePlan) (evaluatedFront, error)) (evaluatedFront, error) {
	result, err := evaluate(plan)
	if err != nil || result.state != "ready" {
		return result, err
	}
	seen := map[int64]bool{plan.Pull.Number: true}
	parentNumber, parentSHA := plan.ParentPullNumber, plan.ParentSHA
	for parentNumber != 0 {
		parent := byNumber[parentNumber]
		reason := ""
		switch {
		case seen[parentNumber]:
			reason = "parent_cycle"
		case parent == nil:
			reason = "parent_source_unavailable"
		case parent.Pull.HeadSHA != parentSHA:
			reason = "parent_moved"
		case parent.Repository.ID != plan.Repository.ID || !sameTarget(parent.Pull, plan.Repository.ID, parentNumber):
			reason = "parent_identity_changed"
		case parent.SourceReason != "":
			reason = parent.SourceReason
		case parent.Pull.IsDraft != nil && *parent.Pull.IsDraft:
			reason = "pull_draft"
		case parent.Pull.State != "open" || parent.Pull.Merged:
			reason = "parent_unavailable"
		}
		if reason != "" {
			return evaluatedFront{state: "held", reason: "predecessor_unfinished", plan: plan, blockingPull: parentNumber, blockingReason: reason}, nil
		}
		seen[parentNumber] = true
		// The parent supplies source identity, not review completion or allowance.
		// Its findings, rounds and PR pause remain obligations on that parent.
		parentNumber, parentSHA = parent.ParentPullNumber, parent.ParentSHA
	}
	return result, nil
}

func (e *Engine) evaluateFront(ctx context.Context, q *sql.Tx, r Run, repo int64, prefix []SourcePlan, complete bool) (evaluatedFront, error) {
	byNumber := map[int64]*SourcePlan{}
	for i := range prefix {
		byNumber[prefix[i].Pull.Number] = &prefix[i]
	}
	var firstHeld *evaluatedFront
	for i := range prefix {
		plan := &prefix[i]
		result, err := e.evaluateCandidatePlan(ctx, q, r, repo, plan, byNumber)
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
			return result, nil
		}
		if result.state == "held" && plan.Pull.Number == r.ExpectedPullNumber {
			// With no earlier ready PR, report the asserted target's own hold.
			result.plan = plan
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
