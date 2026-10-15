package reviewexec

import (
	"context"
	"slices"
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

type SourcePlan struct {
	Repository       reviewprovider.Repository
	Pull             reviewprovider.Pull
	ParentPullNumber int64
	ParentSHA        string
	ObservedAt       string
}

// ResolveSource refreshes only the asserted target and preceding PR facts
// needed to find its effective parent. This supplies source facts, not review
// policy, activity, allowance or permission to dispatch an external action.
func ResolveSource(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, request Request) (SourcePlan, error) {
	if reader == nil || repositoryID <= 0 || !sha.MatchString(request.HeadSHA) || !validChannel(request.Channel) || !slices.Contains(c.Channels, request.Channel) || c.Paused || c.Mode != "independent" && c.Mode != "stacked" || len(c.Queue) == 0 || len(c.Queue) > 100 {
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
	position := slices.Index(c.Queue, request.PullNumber)
	if position < 0 {
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
	if c.Mode == "stacked" {
		for i := position - 1; i >= 0; i-- {
			parent, err := reader.Pull(ctx, repo.FullName, c.Queue[i])
			if err != nil {
				return SourcePlan{}, err
			}
			if !sameTarget(parent, repositoryID, c.Queue[i]) {
				return SourcePlan{}, sourceHold("parent_identity_changed")
			}
			if parent.Merged {
				continue
			}
			if parent.State != "open" {
				return SourcePlan{}, sourceHold("parent_unavailable")
			}
			plan.ParentPullNumber = parent.Number
			plan.ParentSHA = parent.HeadSHA
			break
		}
	}
	if plan.ParentPullNumber == 0 && pull.BaseRef != repo.DefaultBranch {
		return SourcePlan{}, sourceHold("default_base_changed")
	}
	if plan.ParentSHA != pull.BaseSHA {
		return SourcePlan{}, sourceHold("parent_moved")
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
