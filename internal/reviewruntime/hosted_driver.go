package reviewruntime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// HostedProvider is bound by the operator, never by a review request. Its
// activity observation is separate from the final review-evidence collector.
type HostedProvider interface {
	reviewexec.TargetReader
	HostedAllowanceProvider
	HostedRecoveryReader
	CurrentActor(context.Context) (reviewprovider.Author, error)
	ObserveHostedActivity(context.Context, string, string, reviewprovider.HostedActivitySelector) (reviewprovider.HostedActivity, error)
}

// HostedDriver composes journaled preflight, one-use dispatch and retained
// observation. Registering this component with a daemon is a separate step.
type HostedDriver struct {
	store      *core.Store
	engine     *reviewexec.Engine
	capability OperatorCapability
	manager    *reviewworkspace.Manager
	captures   *HostedCaptureStore
	provider   HostedProvider
	fetch      func(context.Context, int64, string, string, string, string) error
	verify     func(context.Context, reviewworkspace.Workspace) error
	wait       func(context.Context, time.Duration) error
	mu         sync.Mutex
	prepared   map[string]*hostedPrepared
	recovering atomic.Bool
}

type hostedPrepared struct {
	operation     reviewexec.Operation
	configuration reviewexec.Configuration
	plan          reviewexec.SourcePlan
	workspace     reviewworkspace.Workspace
	anchor        reviewexec.Anchor
	postingActor  reviewprovider.Author
	expires       time.Time
}

var _ reviewexec.Driver = (*HostedDriver)(nil)
var _ HostedProvider = (*reviewprovider.GitHub)(nil)

func (*HostedDriver) String() string     { return "HostedDriver{private}" }
func (d *HostedDriver) GoString() string { return d.String() }

func NewHostedDriver(store *core.Store, engine *reviewexec.Engine, capability OperatorCapability, manager *reviewworkspace.Manager, captures *HostedCaptureStore, provider HostedProvider) (*HostedDriver, error) {
	if store == nil || engine == nil || manager == nil || captures == nil || provider == nil || reflect.ValueOf(provider).Kind() == reflect.Pointer && reflect.ValueOf(provider).IsNil() || !validIdentifier(capability.ID) || !validIdentifier(capability.Account) || !validOrganization(capability.Organization) || !validExecutable(capability.GitExecutable) || !slices.Contains(capability.Channels, "hosted") || len(capability.Channels) > 2 || !validHostedSettings(capability.Hosted) || capability.GitHubCredential() == "" || len(capability.GitHubCredential()) > maxCredentialTokenBytes || containsWhitespaceOrControl(capability.GitHubCredential()) {
		return nil, configurationError()
	}
	seen := map[string]bool{}
	for _, channel := range capability.Channels {
		if channel != "cli" && channel != "hosted" || seen[channel] {
			return nil, configurationError()
		}
		seen[channel] = true
	}
	fingerprint, err := capabilityFingerprint(capability)
	if err != nil || fingerprint != capability.Fingerprint || !engine.MatchesCapability(reviewexec.Capability{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: capability.Fingerprint}) {
		return nil, configurationError()
	}
	capability.Channels = slices.Clone(capability.Channels)
	settings := *capability.Hosted
	capability.Hosted = &settings
	return &HostedDriver{store: store, engine: engine, capability: capability, manager: manager, captures: captures, provider: provider, fetch: manager.Fetch, verify: manager.Verify, wait: waitHosted, prepared: map[string]*hostedPrepared{}}, nil
}

func waitHosted(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *HostedDriver) providerActor() reviewprovider.Author {
	return reviewprovider.Author{ID: d.capability.Hosted.ProviderActorID, Login: d.capability.Hosted.ProviderLogin, Kind: "Bot"}
}

func (d *HostedDriver) checkIdle(ctx context.Context, plan reviewexec.SourcePlan) error {
	settings := d.capability.Hosted
	observation, err := d.provider.ObserveHostedActivity(ctx, plan.Repository.FullName, plan.Pull.HeadSHA, reviewprovider.HostedActivitySelector{Mode: settings.ProgressMode, ProviderActorID: settings.ProviderActorID, AppID: settings.ProgressAppID})
	if err != nil {
		return reviewHold("provider_activity_unproven")
	}
	if observation.State == "active" {
		return reviewHold("provider_review_active")
	}
	if observation.State != "idle" || observation.ObservedAt.IsZero() || observation.ObservedAt.Location() != time.UTC || time.Since(observation.ObservedAt) > time.Minute || observation.ObservedAt.After(time.Now().Add(5*time.Second)) {
		return reviewHold("provider_activity_unproven")
	}
	return nil
}

func (d *HostedDriver) purgePrepared() {
	now := time.Now()
	for id, prepared := range d.prepared {
		if prepared != nil && !now.Before(prepared.expires) {
			delete(d.prepared, id)
		}
	}
}

func (d *HostedDriver) Prepare(ctx context.Context, operation reviewexec.Operation, configuration reviewexec.Configuration, attempt reviewexec.Attempt) (preparation reviewexec.Preparation, failure error) {
	if ctx == nil {
		return preparation, reviewHold("execution_binding_changed")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	current, err := d.engine.ExecutionContext(ctx, operation.ProjectID, operation.ID)
	if err != nil {
		return preparation, err
	}
	if !sameOperation(operation, current.Operation) || !reflect.DeepEqual(configuration, current.Configuration) || configuration.CapabilityID != d.capability.ID || operation.Channel != "hosted" || attempt.Provider != "coderabbit" || attempt.Account != d.capability.Account || attempt.ProjectID != operation.ProjectID || attempt.OperationID != operation.ID || attempt.ID != current.Operation.AttemptID || attempt.State != "preparing" || attempt.Revision != 1 {
		return preparation, reviewHold("execution_binding_changed")
	}
	d.mu.Lock()
	d.purgePrepared()
	_, exists := d.prepared[attempt.ID]
	if !exists {
		d.prepared[attempt.ID] = nil
	}
	d.mu.Unlock()
	if exists {
		return preparation, reviewHold("preparation_duplicate")
	}
	defer func() {
		if failure != nil {
			d.mu.Lock()
			delete(d.prepared, attempt.ID)
			d.mu.Unlock()
		}
	}()
	reads := newSourceReads(d.provider)
	plan, _, err := resolveReviewSource(ctx, d.engine, reads, current.RepositoryID, current.Configuration, current.Operation)
	if err != nil {
		return preparation, err
	}
	if err := d.checkIdle(ctx, plan); err != nil {
		return preparation, err
	}
	if err := d.fetch(ctx, current.RepositoryID, plan.Repository.FullName, plan.Pull.HeadSHA, plan.Pull.BaseSHA, d.capability.GitHubCredential()); err != nil {
		return preparation, reviewHold("source_unavailable")
	}
	workspace, err := d.manager.Prepare(ctx, current.RepositoryID, attempt.ID, plan.Pull.HeadSHA, plan.Pull.BaseSHA)
	if err != nil {
		return preparation, reviewHold("workspace_unavailable")
	}
	workspace, err = d.manager.BindRepository(ctx, workspace, plan.Repository.FullName)
	if err != nil {
		return preparation, reviewHold("workspace_unavailable")
	}
	if err := d.verify(ctx, workspace); err != nil {
		return preparation, reviewHold("workspace_changed")
	}
	paths, err := d.manager.ChangedFiles(ctx, workspace)
	if err != nil || len(paths) == 0 {
		return preparation, reviewHold("comparison_unavailable")
	}
	if ceiling := current.Policy.Policy.MaxReviewFiles; ceiling > 0 && int64(len(paths)) > ceiling {
		return preparation, reviewHold("project_file_limit")
	}
	anchor := reviewexec.Anchor{RepositoryID: current.RepositoryID, Repository: plan.Repository.FullName, PullNumber: plan.Pull.Number, CandidateSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, ParentSHA: plan.ParentSHA, ParentPullNumber: plan.ParentPullNumber, BaseRef: plan.Pull.BaseRef, MergeBaseSHA: workspace.MergeBaseSHA, PatchSHA256: workspace.PatchSHA256, ObservedAt: plan.ObservedAt}
	actor, err := d.provider.CurrentActor(ctx)
	if err != nil {
		return preparation, reviewHold("provider_actor_unproven")
	}
	var observed ObservedHostedAllowance
	for pass := 0; pass < 8; pass++ {
		observed, err = ObserveHostedAllowance(ctx, d.provider, d.captures, d.engine, attempt, anchor, plan.Repository, actor, d.providerActor())
		if err != nil {
			return preparation, reviewHold("hosted_allowance_unproven")
		}
		if observed.Observation.State == "available_observed" {
			break
		}
		if observed.Observation.State == "cooldown_observed" {
			return preparation, reviewHold("hosted_allowance_cooldown")
		}
		if observed.Reason != "allowance_not_proven" || pass == 7 {
			return preparation, reviewHold("hosted_allowance_unproven")
		}
		if err := d.wait(ctx, 2*time.Second); err != nil {
			return preparation, err
		}
	}
	if err := RetainHostedAllowance(ctx, d.captures, d.engine, attempt, anchor, int64(len(paths)), observed.Capture); err != nil {
		return preparation, reviewHold("hosted_allowance_unproven")
	}
	_, err = d.engine.PublishAttempt(ctx, operation.ProjectID, attempt.ID, nil)
	if err != nil {
		var missing *core.Error
		if !errors.As(err, &missing) || missing.Code != "review_projection_missing" {
			return preparation, err
		}
		metadata := reviews.PullInput{Number: plan.Pull.Number, Title: plan.Pull.Title, State: plan.Pull.State, HeadSHA: plan.Pull.HeadSHA, BaseRef: plan.Pull.BaseRef, BaseSHA: plan.Pull.BaseSHA, CIState: "unknown", Mergeability: "unknown", Complete: false}
		if _, err = d.engine.PublishAttempt(ctx, operation.ProjectID, attempt.ID, &metadata); err != nil {
			return preparation, err
		}
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: operation.ID, AttemptID: attempt.ID, HeadSHA: operation.HeadSHA})
	if err != nil {
		return preparation, err
	}
	d.mu.Lock()
	d.prepared[attempt.ID] = &hostedPrepared{operation: current.Operation, configuration: current.Configuration, plan: plan, workspace: workspace, anchor: anchor, postingActor: actor, expires: time.Now().Add(time.Minute)}
	d.mu.Unlock()
	return reviewexec.Preparation{Anchor: anchor, Intent: intent}, nil
}

func (d *HostedDriver) Dispatch(ctx context.Context, operation reviewexec.Operation, attempt reviewexec.Attempt, recordTrigger func(context.Context, string) error) (reviewexec.Result, error) {
	d.mu.Lock()
	d.purgePrepared()
	prepared := d.prepared[attempt.ID]
	if prepared != nil {
		delete(d.prepared, attempt.ID)
	}
	d.mu.Unlock()
	failed := func(reason string) (reviewexec.Result, error) {
		return reviewexec.Result{State: "failed", Reason: reason}, nil
	}
	if prepared == nil {
		return failed("preparation_missing")
	}
	if ctx == nil || recordTrigger == nil || !sameOperation(operation, prepared.operation) || attempt.ProjectID != operation.ProjectID || attempt.OperationID != operation.ID || attempt.Provider != "coderabbit" || attempt.Account != d.capability.Account || attempt.State != "posting" || attempt.Anchor == nil || *attempt.Anchor != prepared.anchor {
		return failed("execution_binding_changed")
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: operation.ID, AttemptID: attempt.ID, HeadSHA: operation.HeadSHA}
	body, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil || attempt.Intent != body || attempt.ExternalID != "" {
		return failed("execution_binding_changed")
	}
	reads := newSourceReads(d.provider)
	if err := reviewexec.RecheckSource(ctx, reads, prepared.anchor.RepositoryID, prepared.configuration, operationRequest(operation), prepared.plan); err != nil {
		return failed("source_changed")
	}
	_, predecessors, err := resolveReviewSource(ctx, d.engine, reads, prepared.anchor.RepositoryID, prepared.configuration, operation)
	if err != nil {
		return failed("source_admission_changed")
	}
	if err := d.verify(ctx, prepared.workspace); err != nil {
		return failed("workspace_changed")
	}
	actor, err := d.provider.CurrentActor(ctx)
	if err != nil || actor.ID != prepared.postingActor.ID || actor.Kind != prepared.postingActor.Kind {
		return failed("provider_actor_changed")
	}
	if err := d.checkIdle(ctx, prepared.plan); err != nil {
		return failed("provider_activity_changed")
	}
	bound, err := d.engine.ReadHostedRecovery(ctx, operation.ProjectID, attempt.ID, d.capability.ID)
	if err != nil || bound.Attempt.Revision != attempt.Revision || bound.Attempt.Intent != body {
		return failed("execution_binding_changed")
	}
	if err := d.engine.VerifyHostedAction(ctx, operation.ProjectID, attempt.ID, attempt.Revision, predecessors...); err != nil {
		return failed("admission_changed")
	}
	comment, err := d.provider.PostIssueComment(ctx, prepared.plan.Repository.FullName, operation.PullNumber, body)
	if err != nil {
		var failure *reviewprovider.Error
		if errors.As(err, &failure) && !failure.OutcomeUnknown {
			return failed("provider_post_rejected")
		}
		return reviewexec.Result{State: "uncertain", Reason: "trigger_post_unproven"}, nil
	}
	trigger, err := reviewprovider.RecoverHostedTrigger([]reviewprovider.IssueComment{comment}, true, intent, prepared.postingActor, bound.EarliestPosting, time.Now().UTC())
	if err != nil || trigger.State != "adopted" {
		return reviewexec.Result{State: "uncertain", Reason: "trigger_post_unproven"}, nil
	}
	externalID := strconv.FormatInt(trigger.CommentID, 10)
	retentionCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = recordTrigger(retentionCtx, externalID)
	cancel()
	if err != nil {
		return reviewexec.Result{State: "uncertain", Reason: "trigger_recording_pending", ExternalID: externalID}, nil
	}
	// Supervision waits outside agent calls and SQL transactions. Each bounded
	// pass retains source before interpretation and never reposts the command.
	for pass := 0; pass < 80; pass++ {
		observed, err := observeHostedAttempt(ctx, d.provider, d.captures, d.engine, operation.ProjectID, attempt.ID, d.capability.ID, prepared.postingActor, d.providerActor(), false)
		if err != nil {
			return reviewexec.Result{State: "uncertain", Reason: "hosted_observation_pending", ExternalID: externalID}, nil
		}
		if observed.State == "completed" {
			return reviewexec.Result{State: "completed", Reason: "hosted_evidence_retained", ExternalID: externalID}, nil
		}
		if observed.Reason == "hosted_observation_capacity" {
			return reviewexec.Result{State: "uncertain", Reason: observed.Reason, ExternalID: externalID}, nil
		}
		if observed.Reason == "conflicting_trigger_evidence" || observed.Reason == "trigger_identity_changed" {
			return reviewexec.Result{State: "uncertain", Reason: "hosted_observation_conflict", ExternalID: externalID}, nil
		}
		if pass == 79 || d.wait(ctx, 15*time.Second) != nil {
			break
		}
	}
	return reviewexec.Result{State: "uncertain", Reason: "hosted_observation_pending", ExternalID: externalID}, nil
}
