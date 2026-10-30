package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// ExecutionContext is trusted local admission state, not dispatch permission.
// Source identity, fresh allowance and workspace checks remain the driver's job.
type ExecutionContext struct {
	Operation     Operation
	Configuration Configuration
	Policy        PolicyRecord
	RepositoryID  int64
}

func boundPolicy(ctx context.Context, tx *sql.Tx, operation Operation) (PolicyRecord, string, error) {
	if operation.PolicyRevision < 1 {
		return PolicyRecord{}, "policy_unbound", nil
	}
	policy, err := readPolicy(ctx, tx, operation.ProjectID)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
			return PolicyRecord{}, "policy_unconfigured", nil
		}
		return PolicyRecord{}, "", err
	}
	if policy.Revision != operation.PolicyRevision {
		return policy, "policy_changed", nil
	}
	return policy, "", nil
}

func policyAdmissionConflict() error {
	return core.Fail("review_conflict", "The bound project review policy is unavailable or changed; request review again.", 409)
}

func (e *Engine) currentExecutionContext(ctx context.Context, tx *sql.Tx, operation Operation) (ExecutionContext, error) {
	result := ExecutionContext{Operation: operation}
	if err := requireSourceObligationsClear(ctx, tx, operation.ProjectID, operation.PullNumber); err != nil {
		return ExecutionContext{}, err
	}
	if err := requireIncomingRoutesClear(ctx, tx, operation.ProjectID, operation.PullNumber); err != nil {
		return ExecutionContext{}, err
	}
	configuration, revision, err := readConfiguration(ctx, tx, operation.ProjectID)
	if err != nil {
		return ExecutionContext{}, err
	}
	compatible, err := compatibleQueuedConfiguration(ctx, tx, operation, configuration, revision)
	if err != nil {
		return ExecutionContext{}, err
	}
	if !compatible || e.validateConfiguration(configuration) != nil || !slices.Contains(configuration.Queue, operation.PullNumber) || !slices.Contains(configuration.Channels, operation.Channel) {
		return ExecutionContext{}, conflict()
	}
	capability := e.capabilities[configuration.CapabilityID]
	if operation.CapabilityFingerprint != capability.Fingerprint {
		return ExecutionContext{}, conflict()
	}
	allowed, err := actorAllowed(ctx, tx, operation.ProjectID, operation.ActorID, operation.WorkerID)
	if err != nil {
		return ExecutionContext{}, err
	}
	if !allowed {
		return ExecutionContext{}, core.Fail("forbidden", "The review actor or worker no longer has project access.", 403)
	}
	policy, reason, err := boundPolicy(ctx, tx, operation)
	if err != nil {
		return ExecutionContext{}, err
	}
	if reason != "" {
		return ExecutionContext{}, policyAdmissionConflict()
	}
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, operation.ProjectID).Scan(&result.RepositoryID); err != nil {
		return ExecutionContext{}, err
	}
	if result.RepositoryID < 1 {
		return ExecutionContext{}, conflict()
	}
	if _, err := admitRoundControl(ctx, tx, operation); err != nil {
		return ExecutionContext{}, err
	}
	// Compatibility authorizes the original request; it never replaces its
	// source-resolution plan with the current queue's configuration.
	original, err := sourceConfiguration(ctx, tx, operation.ProjectID, operation.ConfigurationRevision)
	if err != nil {
		return ExecutionContext{}, err
	}
	result.Configuration, result.Policy = original, policy
	return result, nil
}

// readAdmission uses one finite read-only snapshot without retaining credentials
// or changing any journal state. These methods are for trusted in-process use.
func (e *Engine) readAdmission(ctx context.Context, read func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		defer tx.Rollback()
		err = read(ctx, tx)
	}
	if ctx.Err() != nil {
		return core.Fail("overloaded", "Local review verification could not finish within its bounded request; retry the local check.", 503)
	}
	return core.NormalizeStorageError(err)
}

// ExecutionContext rechecks a preparing operation before expensive source work.
// It does not start or authorize a provider action.
func (e *Engine) ExecutionContext(ctx context.Context, project, operationID string) (ExecutionContext, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(operationID) {
		return ExecutionContext{}, invalid()
	}
	var result ExecutionContext
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		operation, err := readOperation(ctx, tx, project, operationID)
		if err != nil {
			return err
		}
		if operation.State != "preparing" {
			return conflict()
		}
		result, err = e.currentExecutionContext(ctx, tx, operation)
		return err
	})
	if err != nil {
		return ExecutionContext{}, err
	}
	return result, nil
}

// VerifyAction is the final local journal guard. It complements fresh source,
// allowance and workspace verification; it is not sufficient dispatch authority.
func (e *Engine) VerifyAction(ctx context.Context, project, attemptID string, expectedRevision int64, predecessors ...SourcePlan) error {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) || expectedRevision < 1 {
		return invalid()
	}
	return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return e.verifyAction(ctx, tx, project, attemptID, expectedRevision, predecessors)
	})
}

// verifyAction shares one read snapshot with adapter-specific local proof.
func (e *Engine) verifyAction(ctx context.Context, tx *sql.Tx, project, attemptID string, expectedRevision int64, predecessors []SourcePlan) error {
	attempt, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return err
	}
	if attempt.Revision != expectedRevision {
		return conflict()
	}
	operation, err := readOperation(ctx, tx, project, attempt.OperationID)
	if err != nil {
		return err
	}
	if operation.AttemptID != attempt.ID || operation.State != attempt.State || !(operation.Channel == "cli" && attempt.State == "active" || operation.Channel == "hosted" && attempt.State == "posting") {
		return conflict()
	}
	current, err := e.currentExecutionContext(ctx, tx, operation)
	if err != nil {
		return err
	}
	capability := e.capabilities[current.Configuration.CapabilityID]
	if attempt.Provider != capability.Provider || attempt.Account != capability.Account || attempt.Anchor == nil || !validEvidenceAnchor(*attempt.Anchor) || attempt.Anchor.RepositoryID != current.RepositoryID || attempt.Anchor.PullNumber != operation.PullNumber || attempt.Anchor.CandidateSHA != operation.HeadSHA {
		return conflict()
	}
	var reserved int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND repository_id=? AND pull_number=? AND action_class=? AND account=?`, attempt.ID, attempt.Provider, current.RepositoryID, operation.PullNumber, operation.Channel, attempt.Account).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 1 {
		return conflict()
	}
	if err := verifyPredecessors(ctx, tx, project, current, *attempt.Anchor, predecessors); err != nil {
		return err
	}
	state, err := channelProgress(ctx, tx, project, current.RepositoryID, operation.PullNumber, operation.Channel)
	if err != nil {
		return err
	}
	if state.Pending {
		return core.Fail("review_evidence_pending", "Resolve retained native evidence in the selected channel before starting another review.", 409)
	}
	rounds, err := admitRoundControl(ctx, tx, operation)
	if err != nil {
		return err
	}
	if state.Covered && !rounds {
		return &core.Error{Code: "review_held", Message: "This PR already has a completed review in the selected channel.", Status: 409, Details: map[string]string{"reason": "target_already_covered"}}
	}
	return nil
}

// verifyPredecessors uses the final action's existing SQL snapshot. Fresh
// provider facts are supplied by the driver; no provider work occurs here.
func verifyPredecessors(ctx context.Context, tx *sql.Tx, project string, current ExecutionContext, target Anchor, predecessors []SourcePlan) error {
	if current.Operation.AllowOutOfOrder || current.Configuration.Mode == "independent" {
		if len(predecessors) != 0 {
			return invalid()
		}
		return nil
	}
	if !slices.Contains(current.Configuration.Queue, current.Operation.PullNumber) || len(predecessors) > 100 {
		return invalid()
	}
	number, head := target.ParentPullNumber, target.ParentSHA
	seen := map[int64]bool{current.Operation.PullNumber: true}
	for _, plan := range predecessors {
		if number == 0 || seen[number] || !slices.Contains(current.Configuration.Queue, number) || plan.Pull.Number != number || plan.Pull.HeadSHA != head {
			return invalid()
		}
		seen[number] = true
		observed, err := time.Parse(time.RFC3339Nano, plan.ObservedAt)
		if err != nil || observed.IsZero() || plan.Repository.ID != current.RepositoryID || !repository.MatchString(plan.Repository.FullName) || plan.Repository.DefaultBranch == "" || !safeText(plan.Repository.DefaultBranch, 256) || !sameTarget(plan.Pull, current.RepositoryID, number) || !validCoverageSource(plan) || plan.Pull.Merged {
			return invalid()
		}
		if err := requireSourceObligationsClear(ctx, tx, project, number); err != nil {
			return err
		}
		routes, err := pendingIncomingRoutes(ctx, tx, project, number)
		if err != nil {
			return err
		}
		if routes > 0 {
			return predecessorUnfinishedHold(number, "incoming_route_pending")
		}
		state, err := channelProgress(ctx, tx, project, current.RepositoryID, number, current.Operation.Channel)
		if err != nil {
			return err
		}
		if state.Pending || state.RoundsConfigured && !state.RoundsComplete || !state.RoundsConfigured && !state.Covered {
			return predecessorUnfinishedHold(number, "discovery_incomplete")
		}
		number, head = plan.ParentPullNumber, plan.ParentSHA
	}
	if number != 0 {
		return invalid()
	}
	return nil
}

func predecessorUnfinishedHold(number int64, cause string) error {
	return &core.Error{Code: "review_held", Message: "An ancestor has not completed discovery in the selected channel.", Status: 409, Details: map[string]string{"reason": "predecessor_unfinished", "pull_number": strconv.FormatInt(number, 10), "cause": cause}}
}
