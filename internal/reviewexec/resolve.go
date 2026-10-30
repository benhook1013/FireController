package reviewexec

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// TargetReader is the read-only provider boundary required to select an exact
// source comparison. It cannot post a command or spend a review.
type TargetReader interface {
	Repository(context.Context, int64) (reviewprovider.Repository, error)
	Pull(context.Context, string, int64) (reviewprovider.Pull, error)
}

// ParentReader resolves a published base branch to its unique open PR in the
// same repository. Queue position never supplies repository dependency identity.
type ParentReader interface {
	ParentPull(context.Context, string, string) (reviewprovider.Pull, error)
}

type SourcePlan struct {
	Repository       reviewprovider.Repository
	Pull             reviewprovider.Pull
	ParentPullNumber int64
	ParentSHA        string
	ObservedAt       string
	SourceReason     string
}

// ResolveSource refreshes only the asserted target and its actual base-branch
// parent when present. This supplies source facts, not review
// policy, activity, allowance or permission to dispatch an external action.
func ResolveSource(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, request Request) (SourcePlan, error) {
	return resolveSource(ctx, reader, repositoryID, c, request, true)
}

// ResolveSourceObservation selects the same exact comparison without requiring
// execution to be enabled. Observing source spends no review and cannot confer
// permission to dispatch one; channel pauses do not prevent explicit reads.
func ResolveSourceObservation(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, number int64, head string) (SourcePlan, error) {
	return resolveSource(ctx, reader, repositoryID, c, Request{PullNumber: number, HeadSHA: head}, false)
}

func resolveSource(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, request Request, execution bool) (SourcePlan, error) {
	if ctx == nil || reader == nil || repositoryID <= 0 || !sha.MatchString(request.HeadSHA) || execution && (!validChannel(request.Channel) || !slices.Contains(c.Channels, request.Channel) || c.Paused) || c.Mode != "independent" && c.Mode != "stacked" || len(c.Queue) == 0 || len(c.Queue) > 100 || len(c.Channels) == 0 || len(c.Channels) > 2 {
		return SourcePlan{}, invalid()
	}
	seen := map[int64]bool{}
	for _, number := range c.Queue {
		if number < 1 || number > 2147483647 || seen[number] {
			return SourcePlan{}, invalid()
		}
		seen[number] = true
	}
	channels := map[string]bool{}
	for _, channel := range c.Channels {
		if !validChannel(channel) || channels[channel] {
			return SourcePlan{}, invalid()
		}
		channels[channel] = true
	}
	if !slices.Contains(c.Queue, request.PullNumber) {
		return SourcePlan{}, core.Fail("review_not_configured", "The asserted review target is not configured.", 409)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	observed := stamp()
	repo, err := reader.Repository(ctx, repositoryID)
	if err != nil {
		return SourcePlan{}, err
	}
	if repo.ID != repositoryID || !repository.MatchString(repo.FullName) || repo.DefaultBranch == "" {
		return SourcePlan{}, sourceHold("repository_changed")
	}
	pull, err := reader.Pull(ctx, repo.FullName, request.PullNumber)
	if err != nil {
		return SourcePlan{}, err
	}
	if !sameTarget(pull, repositoryID, request.PullNumber) || pull.State != "open" || pull.Merged || pull.HeadSHA != request.HeadSHA || pull.ChangedFiles < 1 {
		return SourcePlan{}, sourceHold("target_changed")
	}
	plan := SourcePlan{Repository: repo, Pull: pull, ParentSHA: pull.BaseSHA, ObservedAt: observed}
	if c.Mode == "stacked" && pull.BaseRef != repo.DefaultBranch {
		parents, ok := reader.(ParentReader)
		if !ok {
			return SourcePlan{}, unavailableParentHold(pull)
		}
		parent, err := parents.ParentPull(ctx, repo.FullName, pull.BaseRef)
		if err != nil {
			var hold *core.Error
			if errors.As(err, &hold) && hold.Code == "review_held" {
				if details, ok := hold.Details.(map[string]string); ok && details["reason"] == "parent_unavailable" {
					return SourcePlan{}, unavailableParentHold(pull)
				}
			}
			return SourcePlan{}, err
		}
		if parent.Number <= 0 || parent.State != "open" || parent.Merged {
			return SourcePlan{}, unavailableParentHold(pull)
		}
		if !sameTarget(parent, repositoryID, parent.Number) || parent.Number == pull.Number || parent.HeadRef != pull.BaseRef {
			return SourcePlan{}, sourceHold("parent_identity_changed")
		}
		plan.ParentPullNumber, plan.ParentSHA = parent.Number, parent.HeadSHA
	}
	if plan.ParentPullNumber == 0 && pull.BaseRef != repo.DefaultBranch {
		return SourcePlan{}, sourceHold("default_base_changed")
	}
	if plan.ParentSHA != pull.BaseSHA {
		return SourcePlan{}, &core.Error{Code: "review_held", Message: "The selected parent and observed base differ.", Status: 409, Details: map[string]string{"reason": "parent_moved", "pull_number": strconv.FormatInt(pull.Number, 10), "parent_pull_number": strconv.FormatInt(plan.ParentPullNumber, 10), "observed_base_sha": pull.BaseSHA, "expected_parent_sha": plan.ParentSHA}}
	}
	if ctx.Err() != nil {
		return SourcePlan{}, sourceHold("observation_incomplete")
	}
	return plan, nil
}

func sameTarget(p reviewprovider.Pull, repo, number int64) bool {
	return p.RepositoryID == repo && p.Number == number && sha.MatchString(p.HeadSHA) && sha.MatchString(p.BaseSHA)
}

func sourceHold(reason string) error {
	return &core.Error{Code: "review_held", Message: "Fresh source facts do not establish the asserted review target.", Status: 409, Details: map[string]string{"reason": reason}}
}

// RecheckSource repeats the required source reads at the final action boundary.
// It never silently adopts a moved candidate, parent or repository identity.
func RecheckSource(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, request Request, prepared SourcePlan) error {
	current, err := ResolveSource(ctx, reader, repositoryID, c, request)
	if err != nil {
		return err
	}
	if current.Repository != prepared.Repository || current.Pull.Number != prepared.Pull.Number || current.Pull.RepositoryID != prepared.Pull.RepositoryID || current.Pull.HeadSHA != prepared.Pull.HeadSHA || current.Pull.BaseSHA != prepared.Pull.BaseSHA || current.Pull.BaseRef != prepared.Pull.BaseRef || current.ParentSHA != prepared.ParentSHA || current.ParentPullNumber != prepared.ParentPullNumber {
		return sourceHold("comparison_changed")
	}
	return nil
}

// The failed lookup may have no parent identity. Retain only facts established
// by the checked child; later provider observations cannot fill this history in.
func unavailableParentHold(pull reviewprovider.Pull) error {
	return &core.Error{Code: "review_held", Message: "The checked PR base branch has no available open parent.", Status: 409, Details: map[string]string{"reason": "parent_unavailable", "pull_number": strconv.FormatInt(pull.Number, 10), "base_ref": pull.BaseRef}}
}
