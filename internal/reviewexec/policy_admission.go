package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"slices"
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
	if slices.Contains(policy.Policy.PausedChannels, operation.Channel) {
		return policy, "policy_paused", nil
	}
	if policy.Revision != operation.PolicyRevision {
		return policy, "policy_changed", nil
	}
	return policy, "", nil
}

func policyAdmissionConflict() error {
	return core.Fail("review_conflict", "The bound project review policy is unavailable, changed or paused; request review again.", 409)
}

func (e *Engine) currentExecutionContext(ctx context.Context, tx *sql.Tx, operation Operation) (ExecutionContext, error) {
	result := ExecutionContext{Operation: operation}
	configuration, revision, err := readConfiguration(ctx, tx, operation.ProjectID)
	if err != nil {
		return ExecutionContext{}, err
	}
	if configuration.Paused || revision != operation.ConfigurationRevision || e.validateConfiguration(configuration) != nil || !slices.Contains(configuration.Queue, operation.PullNumber) || !slices.Contains(configuration.Channels, operation.Channel) {
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
	result.Configuration, result.Policy = configuration, policy
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
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) {
			return err
		}
		return core.Fail("storage_unavailable", "Local review verification is unavailable.", 500)
	}
	return nil
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
func (e *Engine) VerifyAction(ctx context.Context, project, attemptID string, expectedRevision int64) error {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) || expectedRevision < 1 {
		return invalid()
	}
	return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		return nil
	})
}
