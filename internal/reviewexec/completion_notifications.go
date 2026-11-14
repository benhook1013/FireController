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

// publishPartialFindingsNotice is terminal diagnostics, not review completion.
// The immutable capture, not changing adjudication decisions, binds count and deduplication.
func (e *Engine) publishPartialFindingsNotice(ctx context.Context, tx *sql.Tx, operation Operation, provider, state string) (bool, error) {
	if e.publishQueue == nil || (state != "failed" && state != "cancelled" && state != "rate_limited") {
		return false, nil
	}
	facts, err := operationStatusEvidence(ctx, tx, operation)
	if err != nil || facts.ID == "" || facts.Complete {
		return false, err
	}
	var count sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN json_type(body_json,'$.capture.review.Findings')='array' THEN json_array_length(body_json,'$.capture.review.Findings') WHEN json_type(body_json,'$.capture.review.Findings')='null' THEN 0 END FROM review_execution_evidence WHERE id=? AND project_id=? AND operation_id=? AND attempt_id=?`, facts.ID, operation.ProjectID, operation.ID, operation.AttemptID).Scan(&count)
	if err != nil {
		return false, err
	}
	if !count.Valid || count.Int64 < 0 || count.Int64 > 100 {
		return false, adjudicationStorageError()
	}
	if count.Int64 == 0 {
		return false, nil
	}
	label, link, ok := completedReviewContext(provider, facts.Repository, operation.PullNumber)
	if !ok {
		return false, adjudicationStorageError()
	}
	channel := "CLI"
	if operation.Channel == "hosted" {
		channel = "Hosted"
	}
	body := fmt.Sprintf("%s %s partial findings need adjudication for PR #%d.\nRetained findings: %d. Review coverage is incomplete; this is not a completed review round.\nAdjudicate the retained findings and fix accepted issues before normal guarded recovery.\nGitHub: %s\nEvidence: %s\nOperation: %s", label, channel, operation.PullNumber, count.Int64, link, facts.ID, operation.ID)
	scope := core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}
	err = reviews.PublishFollowerNotice(ctx, tx, scope, operation.PullNumber, "review-partial-findings:"+facts.ID, body)
	return err == nil, err
}
