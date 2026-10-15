package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// PolicyAssessment describes current local policy against an immutable captured
// comparison. It does not establish externally current source, CI or readiness.
type PolicyAssessment struct {
	ProjectID      string                    `json:"project_id"`
	EvidenceID     string                    `json:"evidence_id"`
	Anchor         Anchor                    `json:"anchor"`
	Source         string                    `json:"source"`
	PolicyRevision int64                     `json:"policy_revision"`
	EvaluatedAt    string                    `json:"evaluated_at"`
	State          string                    `json:"state"`
	Channels       []PolicyAssessmentChannel `json:"channels"`
}

type PolicyAssessmentChannel struct {
	Channel string `json:"channel"`
	Covered bool   `json:"covered"`
	Pending bool   `json:"pending"`
}

// ReadPolicyAssessment authorizes and evaluates retained evidence and the
// current policy in one bounded snapshot, without refreshing or changing facts.
func (e *Engine) ReadPolicyAssessment(ctx context.Context, scope core.Scope, evidenceID string) (PolicyAssessment, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(scope.ProjectID) || !hostedObservationID(evidenceID, "review_evidence_") {
		return PolicyAssessment{}, invalid()
	}
	var result PolicyAssessment
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		evidence, _, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return err
		}
		// Validate the target even when its own channel is not policy-required.
		// The indexed columns, retained body and actual admitted attempt must all
		// identify the same native source; none is a caller-supplied target.
		var attemptID, operationID string
		if err = tx.QueryRowContext(ctx, `SELECT attempt_id,operation_id FROM review_execution_evidence WHERE project_id=? AND id=?`, scope.ProjectID, evidenceID).Scan(&attemptID, &operationID); err != nil {
			return err
		}
		if !hostedObservationID(attemptID, "review_attempt_") || !hostedObservationID(operationID, "review_operation_") || evidence.AttemptID != attemptID || evidence.OperationID != operationID || !validEvidenceAnchor(evidence.Anchor) || evidence.Capture.ExpectedAnchor != evidence.Anchor || !hash.MatchString(evidence.CapabilityFingerprint) {
			return adjudicationStorageError()
		}
		attempt, err := readAttempt(ctx, tx, scope.ProjectID, attemptID)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "not_found" {
				return adjudicationStorageError()
			}
			return err
		}
		operation, err := readOperation(ctx, tx, scope.ProjectID, operationID)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "not_found" {
				return adjudicationStorageError()
			}
			return err
		}
		if attempt.ID != attemptID || attempt.ProjectID != scope.ProjectID || attempt.OperationID != operationID || operation.ID != operationID || operation.ProjectID != scope.ProjectID || operation.AttemptID != attemptID || operation.ConfigurationRevision < 1 || !identifier.MatchString(operation.ActorID) || operation.WorkerID != "" && !identifier.MatchString(operation.WorkerID) || !identifier.MatchString(attempt.Provider) || !identifier.MatchString(attempt.Account) || !validChannel(operation.Channel) || operation.CapabilityFingerprint != evidence.CapabilityFingerprint || attempt.Anchor == nil || *attempt.Anchor != evidence.Anchor || operation.PullNumber != evidence.Anchor.PullNumber || operation.HeadSHA != evidence.Anchor.CandidateSHA || operation.Channel != evidenceChannel(evidence.Capture) {
			return adjudicationStorageError()
		}
		if operation.Channel == "hosted" && validateHostedEvidenceIdentity(attempt, operation, evidence.Capture) != nil {
			return adjudicationStorageError()
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != evidence.Anchor.RepositoryID {
			return sourceHold("repository_changed")
		}
		result = PolicyAssessment{ProjectID: scope.ProjectID, EvidenceID: evidenceID, Anchor: evidence.Anchor, Source: "captured_comparison", State: "unconfigured", Channels: []PolicyAssessmentChannel{}}
		policy, err := readPolicy(ctx, tx, scope.ProjectID)
		if err != nil {
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "reviews_policy_unconfigured" {
				return err
			}
			result.EvaluatedAt = stamp()
			return nil
		}
		if policy.ProjectID != scope.ProjectID || policy.Revision < 1 {
			return adjudicationStorageError()
		}
		result.PolicyRevision, result.State = policy.Revision, "sufficient"
		for _, channel := range policy.Policy.RequiredChannels {
			state, err := comparison(ctx, tx, scope.ProjectID, repositoryID, evidence.Anchor.PullNumber, evidence.Anchor, channel)
			if err != nil {
				return err
			}
			result.Channels = append(result.Channels, PolicyAssessmentChannel{Channel: channel, Covered: state.Covered, Pending: state.Pending})
			if !state.Covered || state.Pending {
				result.State = "held"
			}
		}
		result.EvaluatedAt = stamp()
		return nil
	})
	if err != nil {
		return PolicyAssessment{}, err
	}
	return result, nil
}
