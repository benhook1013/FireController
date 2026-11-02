package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Called only inside the completed-attempt transaction. Captures, trigger
// acknowledgments and imported history do not imply a usable completed result.
func (e *Engine) publishCompletionNotice(ctx context.Context, tx *sql.Tx, operation Operation, provider string) error {
	if e.publishQueue == nil {
		return nil // Standalone execution journals do not own an inbox workspace.
	}
	facts, err := operationStatusEvidence(ctx, tx, operation)
	if err != nil || !facts.Complete {
		return err
	}
	label, link, ok := completedReviewContext(provider, facts.Repository, operation.PullNumber)
	if !ok {
		return adjudicationStorageError()
	}
	channel := "CLI"
	if operation.Channel == "hosted" {
		channel = "Hosted"
	}
	// Original attempt, admission and evidence identities keep the deduplication
	// body stable when project metadata, findings or the PR title later change.
	body := fmt.Sprintf("%s %s review results ready for PR #%d.\nGitHub: %s\nEvidence: %s\nOperation: %s", label, channel, operation.PullNumber, link, facts.ID, operation.ID)
	scope := core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}
	return reviews.PublishFollowerNotice(ctx, tx, scope, operation.PullNumber, "review-ready:"+facts.ID, body)
}

// completedReviewContext uses original retained identities, never current project
// metadata. Repository dot segments cannot become GitHub navigation paths.
func completedReviewContext(provider, repositoryName string, number int64) (string, string, bool) {
	if !identifier.MatchString(provider) || !repository.MatchString(repositoryName) || number < 1 || number > 2147483647 {
		return "", "", false
	}
	for _, part := range strings.Split(repositoryName, "/") {
		if part == "." || part == ".." {
			return "", "", false
		}
	}
	label := provider
	if provider == "coderabbit" {
		label = "CodeRabbit"
	}
	return label, fmt.Sprintf("https://github.com/%s/pull/%d", repositoryName, number), true
}
