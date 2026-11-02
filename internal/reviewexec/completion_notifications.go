package reviewexec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Called only inside the completed-attempt transaction. Captures, trigger
// acknowledgments and imported history do not imply a usable completed result.
func (e *Engine) publishCompletionNotice(ctx context.Context, tx *sql.Tx, operation Operation) error {
	if e.publishQueue == nil {
		return nil // Standalone execution journals do not own an inbox workspace.
	}
	facts, err := operationStatusEvidence(ctx, tx, operation)
	if err != nil || !facts.Complete {
		return err
	}
	channel := "CLI"
	if operation.Channel == "hosted" {
		channel = "Hosted"
	}
	// Immutable handles keep the per-evidence deduplication body stable when
	// project metadata, findings or the PR title later change.
	body := fmt.Sprintf("%s review results ready for PR #%d.\nEvidence: %s\nOperation: %s", channel, operation.PullNumber, facts.ID, operation.ID)
	scope := core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}
	return reviews.PublishFollowerNotice(ctx, tx, scope, operation.PullNumber, "review-ready:"+facts.ID, body)
}
