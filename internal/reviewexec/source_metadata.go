package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func sourceDisplayPull(plan SourcePlan) reviews.PullInput {
	p := plan.Pull
	title := strings.TrimSpace(p.Title)
	if len(title) > 500 {
		end := 500
		for end > 0 && !utf8.RuneStart(title[end]) {
			end--
		}
		title = title[:end]
	}
	state := p.State
	if p.Merged {
		state = "merged"
	}
	pull := reviews.PullInput{Number: p.Number, Title: title, State: state, HeadSHA: p.HeadSHA, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA, IsDraft: p.IsDraft, MergedAt: p.MergedAt}
	if p.ChangedFiles >= 0 && p.Additions != nil && p.Deletions != nil {
		pull.Diff = &reviews.PullDiff{Files: p.ChangedFiles, Additions: *p.Additions, Deletions: *p.Deletions}
	}
	return pull
}

func publishSourcePlans(ctx context.Context, tx *sql.Tx, scope core.Scope, repo int64, pins map[int64]reviews.SourceMetadataPin, plans []SourcePlan) error {
	if len(plans) == 0 || len(pins) == 0 {
		return nil
	}
	pulls := make([]reviews.PullInput, 0, len(plans))
	for _, plan := range plans {
		if plan.Repository.ID != repo || !sameTarget(plan.Pull, repo, plan.Pull.Number) {
			return invalid()
		}
		pulls = append(pulls, sourceDisplayPull(plan))
	}
	_, err := reviews.PublishSourceMetadata(ctx, tx, scope, repo, pins, pulls)
	return err
}

// Explicit refreshes reuse their independently collected status in the same
// transaction as completion. Ordinary selection metadata retains its own path.
func publishSourceRefreshStatus(ctx context.Context, tx *sql.Tx, scope core.Scope, repo int64, pins map[int64]reviews.SourceMetadataPin, plan SourcePlan, ci *reviewprovider.CIObservation, review *reviewprovider.PullReviewObservation) error {
	if plan.Repository.ID != repo || !sameTarget(plan.Pull, repo, plan.Pull.Number) {
		return invalid()
	}
	mergeability := plan.Pull.Mergeability
	if mergeability != "mergeable" && mergeability != "conflicting" {
		mergeability = "unknown"
	}
	if review != nil && review.State == "complete" && validSourceReviewState(*review, sourcePlanObservation(plan)) {
		mergeability = strings.ToLower(review.Mergeable)
	}
	mergeState := plan.Pull.MergeStateStatus
	if review != nil && review.State == "complete" && validSourceReviewState(*review, sourcePlanObservation(plan)) {
		mergeState = review.MergeStateStatus
	}
	if mergeState == "" {
		mergeState = "UNKNOWN"
	}
	status := reviews.SourceStatus{Pull: sourceDisplayPull(plan), Mergeability: mergeability, MergeStateStatus: mergeState}
	if ci != nil {
		status.CIState = sourceCIQueueState(ci.State)
	}
	_, err := reviews.PublishSourceStatus(ctx, tx, scope, repo, pins, []reviews.SourceStatus{status})
	return err
}
