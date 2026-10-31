package reviewruntime

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// SourceReader exposes only the operator-bound read adapter for explicit source
// refresh work. Selecting it does not prepare or execute a review.
func (d *CLIDriver) SourceReader() reviewexec.TargetReader {
	if d == nil {
		return nil
	}
	return d.reader
}

func (d *HostedDriver) SourceReader() reviewexec.TargetReader {
	if d == nil {
		return nil
	}
	return d.provider
}

func reviewHold(reason string) error {
	return &core.Error{Code: "review_held", Message: "The selected native review is held before execution.", Status: 409, Details: map[string]string{"reason": reason}}
}

func sameOperation(left, right reviewexec.Operation) bool {
	return left.ID == right.ID && left.ProjectID == right.ProjectID && left.ActorID == right.ActorID && left.WorkerID == right.WorkerID && left.ConfigurationRevision == right.ConfigurationRevision && left.PolicyRevision == right.PolicyRevision && left.CapabilityFingerprint == right.CapabilityFingerprint && left.PullNumber == right.PullNumber && left.HeadSHA == right.HeadSHA && left.Channel == right.Channel && left.AllowOutOfOrder == right.AllowOutOfOrder && left.OverrideFileLimit == right.OverrideFileLimit
}

func operationRequest(operation reviewexec.Operation) reviewexec.Request {
	return reviewexec.Request{ExpectedConfigurationRevision: operation.ConfigurationRevision, PullNumber: operation.PullNumber, HeadSHA: operation.HeadSHA, Channel: operation.Channel, AllowOutOfOrder: operation.AllowOutOfOrder, OverrideFileLimit: operation.OverrideFileLimit}
}

func samePreparedSource(left, right reviewexec.SourcePlan) bool {
	return left.Repository == right.Repository && left.Pull.Number == right.Pull.Number && left.Pull.RepositoryID == right.Pull.RepositoryID && left.Pull.HeadSHA == right.Pull.HeadSHA && left.Pull.BaseSHA == right.Pull.BaseSHA && left.Pull.BaseRef == right.Pull.BaseRef && left.ParentSHA == right.ParentSHA && left.ParentPullNumber == right.ParentPullNumber
}

// Slow cold materialization must not turn an aged observation into fresh facts.
// Read the provider again and require the exact comparison we materialized.
func refreshMaterializedSource(ctx context.Context, reader reviewexec.TargetReader, repositoryID int64, configuration reviewexec.Configuration, operation reviewexec.Operation, prepared reviewexec.SourcePlan) (reviewexec.SourcePlan, error) {
	current, err := reviewexec.ResolveSource(ctx, newSourceReads(reader), repositoryID, configuration, operationRequest(operation))
	if err != nil {
		return reviewexec.SourcePlan{}, err
	}
	if !samePreparedSource(current, prepared) {
		return reviewexec.SourcePlan{}, reviewHold("source_changed")
	}
	return current, nil
}

// Slow preparation may age the source beyond the admission window. Fresh
// comparisons need no extra provider reads; aged ones must be verified again.
func refreshAgedMaterializedSource(ctx context.Context, reader reviewexec.TargetReader, repositoryID int64, configuration reviewexec.Configuration, operation reviewexec.Operation, prepared reviewexec.SourcePlan, now time.Time) (reviewexec.SourcePlan, error) {
	observed, err := time.Parse(time.RFC3339Nano, prepared.ObservedAt)
	if err != nil {
		return reviewexec.SourcePlan{}, reviewHold("source_unavailable")
	}
	if !observed.Before(now.Add(-30 * time.Second)) {
		return prepared, nil
	}
	return refreshMaterializedSource(ctx, reader, repositoryID, configuration, operation, prepared)
}

// Each phase has its own memoizer. Repeated stack-resolution reads within one
// phase share facts, while Dispatch cannot reuse Prepare's provider observation.
type sourceReads struct {
	reader       reviewexec.TargetReader
	repositories map[int64]reviewprovider.Repository
	pulls        map[sourcePullKey]reviewprovider.Pull
	parents      map[string]reviewprovider.Pull
}
type sourcePullKey struct {
	repository string
	number     int64
}

func newSourceReads(reader reviewexec.TargetReader) *sourceReads {
	return &sourceReads{reader: reader, repositories: map[int64]reviewprovider.Repository{}, pulls: map[sourcePullKey]reviewprovider.Pull{}, parents: map[string]reviewprovider.Pull{}}
}
func (r *sourceReads) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if value, ok := r.repositories[id]; ok {
		return value, nil
	}
	value, err := r.reader.Repository(ctx, id)
	if err == nil {
		r.repositories[id] = value
	}
	return value, err
}
func (r *sourceReads) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	key := sourcePullKey{repository, number}
	if value, ok := r.pulls[key]; ok {
		return value, nil
	}
	value, err := r.reader.Pull(ctx, repository, number)
	if err == nil {
		r.pulls[key] = value
	}
	return value, err
}

func (r *sourceReads) ParentPull(ctx context.Context, repository, branch string) (reviewprovider.Pull, error) {
	key := repository + "\x00" + branch
	if value, ok := r.parents[key]; ok {
		return value, nil
	}
	reader, ok := r.reader.(reviewexec.ParentReader)
	if !ok {
		return reviewprovider.Pull{}, reviewHold("parent_unavailable")
	}
	value, err := reader.ParentPull(ctx, repository, branch)
	if err == nil {
		r.parents[key] = value
		if value.Number > 0 {
			r.pulls[sourcePullKey{repository, value.Number}] = value
		}
	}
	return value, err
}

func resolveReviewSource(ctx context.Context, engine *reviewexec.Engine, reads *sourceReads, repositoryID int64, configuration reviewexec.Configuration, operation reviewexec.Operation) (reviewexec.SourcePlan, []reviewexec.SourcePlan, error) {
	plan, err := reviewexec.ResolveSource(ctx, reads, repositoryID, configuration, operationRequest(operation))
	if err != nil {
		return plan, nil, err
	}
	comparison, err := engine.Comparison(ctx, operation.ProjectID, plan, operation.Channel)
	if err != nil {
		return plan, nil, err
	}
	if comparison.Pending {
		return plan, nil, core.Fail("review_evidence_pending", "Resolve retained native evidence in the selected channel before starting another review.", 409)
	}
	rounds := false
	if comparison.RoundsConfigured {
		rounds, err = engine.RoundAdmission(ctx, operation.ProjectID, operation.ID)
		if err != nil {
			return plan, nil, err
		}
	}
	if comparison.Covered && !rounds {
		return plan, nil, reviewHold("target_already_covered")
	}
	if configuration.Mode != "stacked" || operation.AllowOutOfOrder {
		return plan, nil, nil
	}

	predecessors := []reviewexec.SourcePlan{}
	seen := map[int64]bool{operation.PullNumber: true}
	number := plan.ParentPullNumber
	for number != 0 {
		if seen[number] || len(predecessors) >= 100 {
			return plan, nil, reviewHold("predecessor_identity_changed")
		}
		seen[number] = true
		if !slices.Contains(configuration.Queue, number) {
			return plan, nil, reviewPredecessorHold(number, "not_in_queue")
		}
		pull, err := reads.Pull(ctx, plan.Repository.FullName, number)
		if err != nil {
			return plan, nil, err
		}
		if pull.RepositoryID != repositoryID || pull.Number != number {
			return plan, nil, reviewHold("predecessor_identity_changed")
		}
		if pull.State != "open" || pull.Merged {
			return plan, nil, reviewHold("predecessor_unavailable")
		}
		request := operationRequest(operation)
		request.PullNumber, request.HeadSHA = number, pull.HeadSHA
		preceding, err := reviewexec.ResolveSource(ctx, reads, repositoryID, configuration, request)
		if err != nil {
			var hold *core.Error
			if errors.As(err, &hold) && hold.Code == "review_held" {
				if details, ok := hold.Details.(map[string]string); ok && details["reason"] == "parent_moved" {
					refusal := map[string]string{"reason": "predecessor_parent_moved"}
					for _, field := range []string{"pull_number", "parent_pull_number", "observed_base_sha", "expected_parent_sha"} {
						refusal[field] = details[field]
					}
					return plan, nil, &core.Error{Code: "review_held", Message: "An ancestor's selected parent and observed base differ.", Status: 409, Details: refusal}
				}
			}
			return plan, nil, err
		}
		covered, err := engine.DiscoveryComplete(ctx, operation.ProjectID, preceding, operation.Channel)
		if err != nil {
			return plan, nil, err
		}
		if !covered {
			return plan, nil, reviewPredecessorHold(number, "discovery_incomplete")
		}
		predecessors = append(predecessors, preceding)
		number = preceding.ParentPullNumber
	}

	return plan, predecessors, nil
}

// prepareReviewWorkspace materializes the already resolved comparison without
// reading provider metadata or obtaining any channel's allowance.
func prepareReviewWorkspace(ctx context.Context, fetch func(context.Context, int64, string, string, string, string) error, verify func(context.Context, reviewworkspace.Workspace) error, manager *reviewworkspace.Manager, credential string, current reviewexec.ExecutionContext, plan reviewexec.SourcePlan, attemptID string) (reviewworkspace.Workspace, int64, error) {
	if err := fetch(ctx, current.RepositoryID, plan.Repository.FullName, plan.Pull.HeadSHA, plan.Pull.BaseSHA, credential); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return reviewworkspace.Workspace{}, 0, reviewHold("source_fetch_timeout")
		}
		return reviewworkspace.Workspace{}, 0, reviewHold("source_unavailable")
	}
	workspace, err := manager.Prepare(ctx, current.RepositoryID, attemptID, plan.Pull.HeadSHA, plan.Pull.BaseSHA)
	if err != nil {
		return reviewworkspace.Workspace{}, 0, reviewHold("workspace_unavailable")
	}
	workspace, err = manager.BindRepository(ctx, workspace, plan.Repository.FullName)
	if err != nil {
		return reviewworkspace.Workspace{}, 0, reviewHold("workspace_unavailable")
	}
	if err := verify(ctx, workspace); err != nil {
		return reviewworkspace.Workspace{}, 0, reviewHold("workspace_changed")
	}
	count, err := manager.ReviewFileCount(ctx, workspace)
	if err != nil || count == 0 {
		return reviewworkspace.Workspace{}, 0, reviewHold("comparison_unavailable")
	}
	if ceiling := current.Policy.Policy.MaxReviewFiles; !current.Operation.OverrideFileLimit && ceiling > 0 && count > ceiling {
		return reviewworkspace.Workspace{}, 0, reviewHold("project_file_limit")
	}
	return workspace, count, nil
}

// publishPreparedAttempt preserves current PR facts. Only an absent projection
// permits initial display metadata from this phase's verified source plan.
func publishPreparedAttempt(ctx context.Context, engine *reviewexec.Engine, project, attemptID string, plan reviewexec.SourcePlan) error {
	_, err := engine.PublishAttempt(ctx, project, attemptID, nil)
	if err == nil {
		return nil
	}
	var missing *core.Error
	if !errors.As(err, &missing) || missing.Code != "review_projection_missing" {
		return err
	}
	metadata := reviews.PullInput{Number: plan.Pull.Number, Title: pullDisplayTitle(plan.Pull.Title), State: plan.Pull.State, HeadSHA: plan.Pull.HeadSHA, BaseRef: plan.Pull.BaseRef, BaseSHA: plan.Pull.BaseSHA, CIState: "unknown", Mergeability: "unknown", Complete: false}
	_, err = engine.PublishAttempt(ctx, project, attemptID, &metadata)
	return err
}

func reviewPredecessorHold(number int64, cause string) error {
	return &core.Error{Code: "review_held", Message: "An ancestor has not completed discovery in the selected channel.", Status: 409, Details: map[string]string{"reason": "predecessor_unfinished", "pull_number": strconv.FormatInt(number, 10), "cause": cause}}
}
