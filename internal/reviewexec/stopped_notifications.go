package reviewexec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/reviews"
)

func stoppedOperationNeedsNotice(state, reason string, interruptedPreparation bool) bool {
	if interruptedPreparation {
		return state == "cancelled" && reason == "supervisor_interrupted"
	}
	if state != "held" && state != "failed" && state != "rate_limited" {
		return false
	}
	// Waiting is determined by the operation state, never by a reason's
	// wording. A terminal allowance/provider refusal has no scheduled retry.
	switch reason {
	case cliDraftWait, "pull_draft", "target_already_covered", "review_round_closed", "review_round_complete", "review_round_capacity", "restore_reconciled", "cli_partial_settled", "target_merged", "target_closed", "target_superseded":
		return false
	}
	return true
}

// Only original operation identity and fixed historical wording enter notices.
// Preparation may have no admitted repository, so do not invent a GitHub link.
func publishStoppedOperationNotice(ctx context.Context, tx *sql.Tx, operation Operation, state, reason string, interruptedPreparation bool) error {
	if !stoppedOperationNeedsNotice(state, reason, interruptedPreparation) {
		return nil
	}
	channel := "CLI"
	if operation.Channel == "hosted" {
		channel = "Hosted"
	}
	description := "This review request stopped without a completed result."
	if state == "held" {
		description = "This review request stopped before review dispatch."
	}
	if interruptedPreparation {
		description = "Preparation was interrupted before review dispatch. The original preparation outcome is unavailable; this does not identify its cause."
	}
	body := fmt.Sprintf("%s review request stopped for PR #%d.\n%s\nOperation: %s\nRead the original operation and current PR state before acting. Acknowledge this notice once handled.", channel, operation.PullNumber, description, operation.ID)
	scope := core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}
	key := "review-stopped:" + operation.ID
	var automatic bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections s JOIN review_automatic_selections a ON a.project_id=s.project_id AND a.selection_id=s.id WHERE s.project_id=? AND s.operation_id=?)`, operation.ProjectID, operation.ID).Scan(&automatic); err != nil {
		return err
	}
	if automatic {
		return reviews.PublishFollowerNotice(ctx, tx, scope, operation.PullNumber, key, body)
	}
	if operation.WorkerID == "" {
		return nil
	}
	preference, err := reviews.ReadFollowing(ctx, tx, operation.ProjectID, operation.WorkerID, operation.PullNumber)
	if err != nil {
		return err
	}
	if preference.Mode != "auto" && preference.Mode != "follow" {
		return nil
	}
	_, err = inbox.PublishControllerMessage(ctx, tx, scope, key, operation.WorkerID, body)
	return err
}

func (e *Engine) holdOperation(ctx context.Context, tx *sql.Tx, operation Operation, reason, at string) error {
	if err := holdOperation(ctx, tx, operation, reason, at); err != nil {
		return err
	}
	if e.publishQueue == nil {
		return nil
	}
	return publishStoppedOperationNotice(ctx, tx, operation, "held", reason, false)
}
