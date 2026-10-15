package reviewruntime

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// HostedRecoveryReader deliberately excludes posting and allowance methods.
// Recovery observes an already admitted action; it cannot start another review.
type HostedRecoveryReader interface {
	Repository(context.Context, int64) (reviewprovider.Repository, error)
	ObserveHosted(context.Context, string, int64) (reviewprovider.HostedPublicationSnapshot, error)
}

// RecoveredHosted reports one bounded recovery pass, not policy sufficiency or
// finalization. Pending findings remain pending after execution completes.
type RecoveredHosted struct {
	State            string `json:"state"`
	Reason           string `json:"reason"`
	EvidenceID       string `json:"evidence_id,omitempty"`
	TriggerCommentID int64  `json:"trigger_comment_id,omitempty"`
}

// RecoverHosted observes exactly one retained attempt through its original
// operator capability. It journals source before adoption, publishes complete
// evidence before settlement, and can resume those local steps after failure.
// The caller owns scheduling; this function does not poll, repost or spend.
func RecoverHosted(ctx context.Context, reader HostedRecoveryReader, captures *HostedCaptureStore, engine *reviewexec.Engine, project, attemptID, capabilityID string, postingActor, providerActor reviewprovider.Author) (RecoveredHosted, error) {
	if ctx == nil || reader == nil || captures == nil || engine == nil {
		return RecoveredHosted{}, core.Fail("validation_failed", "Use the configured hosted recovery capability.", 400)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bound, err := engine.ReadHostedRecovery(ctx, project, attemptID, capabilityID)
	if err != nil {
		return RecoveredHosted{}, err
	}
	attempt := bound.Attempt
	result := RecoveredHosted{State: attempt.State, Reason: "observation_pending", EvidenceID: bound.EvidenceID}
	if attempt.State == "completed" {
		result.Reason = "already_completed"
		return result, nil
	}
	finish := func(evidenceID string) (RecoveredHosted, error) {
		result.EvidenceID = evidenceID
		if _, err := engine.PublishEvidence(ctx, project, evidenceID, nil); err != nil {
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "review_projection_missing" {
				result.Reason = "publication_pending"
				return result, err
			}
			// Current metadata may have been explicitly retired since dispatch.
			// The immutable ledger still proves execution; do not invent PR facts
			// or retain its reservation solely to populate the browser projection.
		}
		next, err := engine.Advance(ctx, project, attempt.ID, attempt.Revision, "completed", "hosted_evidence_recovered", attempt.ExternalID)
		if err != nil {
			result.Reason = "settlement_pending"
			return result, err
		}
		result.State, result.Reason = next.State, "hosted_evidence_recovered"
		return result, nil
	}
	// A retained final capture is authoritative. Do not collect a new artifact
	// that could conflict with the single final slot after a lost local response.
	if bound.EvidenceID != "" {
		return finish(bound.EvidenceID)
	}
	anchor := *attempt.Anchor
	repository, err := reader.Repository(ctx, anchor.RepositoryID)
	if err != nil {
		result.Reason = "source_unavailable"
		return result, err
	}
	if repository.ID != anchor.RepositoryID {
		result.Reason = "repository_changed"
		return result, nil
	}
	started := time.Now().UTC()
	snapshot, err := reader.ObserveHosted(ctx, repository.FullName, anchor.PullNumber)
	finished := time.Now().UTC()
	if err != nil || !snapshot.Complete {
		result.Reason = "observation_incomplete"
		return result, err
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: attempt.OperationID, AttemptID: attempt.ID, HeadSHA: anchor.CandidateSHA}
	record := HostedCaptureRecord{RepositoryID: anchor.RepositoryID, Repository: anchor.Repository, PullNumber: anchor.PullNumber, Intent: intent, PostingActor: postingActor, ProviderActor: providerActor, EarliestPosting: bound.EarliestPosting, StartedAt: started, FinishedAt: finished, Snapshot: snapshot}
	_, retained, err := retainHostedObservation(ctx, captures, engine, attempt, record)
	if err != nil {
		result.Reason = "observation_unavailable"
		return result, err
	}
	trigger, err := reviewprovider.RecoverHostedTrigger(retained.Snapshot.IssueComments, true, intent, retained.PostingActor, retained.EarliestPosting, retained.Snapshot.ObservedAt)
	if err != nil || trigger.State != "adopted" {
		result.Reason = trigger.Reason
		return result, err
	}
	result.TriggerCommentID = trigger.CommentID
	externalID := strconv.FormatInt(trigger.CommentID, 10)
	if attempt.ExternalID != "" && attempt.ExternalID != externalID {
		result.Reason = "trigger_identity_changed"
		return result, nil
	}
	if attempt.State == "posting" || attempt.State == "uncertain" {
		attempt, err = engine.Advance(ctx, project, attempt.ID, attempt.Revision, "active", "trigger_recovered", externalID)
		if err != nil {
			result.Reason = "trigger_recording_pending"
			return result, err
		}
		result.State = attempt.State
	}
	captured, err := RetainHosted(ctx, captures, engine, attempt, retained)
	if err != nil || !captured.Complete {
		result.Reason = captured.Reason
		return result, err
	}
	return finish(captured.Receipt.ID)
}
