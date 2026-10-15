package reviewruntime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// CLIDriver composes one trusted operator capability. Runtime ownership of its
// concrete manager, runner, engine and store is established by the caller.
type CLIDriver struct {
	store      *core.Store
	engine     *reviewexec.Engine
	capability OperatorCapability
	manager    *reviewworkspace.Manager
	captures   *CaptureStore
	reader     reviewexec.TargetReader
	fetch      func(context.Context, int64, string, string, string, string) error
	verify     func(context.Context, reviewworkspace.Workspace) error
	usage      func(context.Context, reviewworkspace.Workspace, string, string) (reviewprocess.Usage, error)
	review     func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error)
	mu         sync.Mutex
	prepared   map[string]*cliPrepared
}

func (*CLIDriver) String() string     { return "CLIDriver{private}" }
func (d *CLIDriver) GoString() string { return d.String() }

type cliPrepared struct {
	operation     reviewexec.Operation
	configuration reviewexec.Configuration
	plan          reviewexec.SourcePlan
	workspace     reviewworkspace.Workspace
	anchor        reviewexec.Anchor
	expires       time.Time
}

var _ reviewexec.Driver = (*CLIDriver)(nil)

func NewCLIDriver(store *core.Store, engine *reviewexec.Engine, capability OperatorCapability, manager *reviewworkspace.Manager, captures *CaptureStore, reader reviewexec.TargetReader, runner *reviewprocess.Runner) (*CLIDriver, error) {
	if store == nil || engine == nil || manager == nil || captures == nil || reader == nil || reflect.ValueOf(reader).Kind() == reflect.Pointer && reflect.ValueOf(reader).IsNil() || runner == nil || !validIdentifier(capability.ID) || !validIdentifier(capability.Account) || !validOrganization(capability.Organization) || !validExecutable(capability.GitExecutable) || !validExecutable(capability.CLIExecutable) || !slices.Contains(capability.Channels, "cli") || len(capability.Channels) > 2 || len(capability.AuthenticationEnvironment) > 7 || len(capability.GitHubCredential()) > maxCredentialTokenBytes || containsWhitespaceOrControl(capability.GitHubCredential()) {
		return nil, configurationError()
	}
	capability.Channels = slices.Clone(capability.Channels)
	environment := make(map[string]string, len(capability.AuthenticationEnvironment))
	for key, value := range capability.AuthenticationEnvironment {
		if !allowedAuthenticationEnvironment[key] || !validAbsolutePath(value) {
			return nil, configurationError()
		}
		environment[key] = value
	}
	capability.AuthenticationEnvironment = environment
	seen := map[string]bool{}
	for _, channel := range capability.Channels {
		if channel != "cli" && channel != "hosted" || seen[channel] {
			return nil, configurationError()
		}
		seen[channel] = true
	}
	if !engine.MatchesCapability(reviewexec.Capability{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: capability.Fingerprint}) {
		return nil, configurationError()
	}
	fingerprint, err := capabilityFingerprint(capability)
	if err != nil || capability.Fingerprint != fingerprint {
		return nil, configurationError()
	}
	return &CLIDriver{store: store, engine: engine, capability: capability, manager: manager, captures: captures, reader: reader, fetch: manager.Fetch, verify: manager.Verify, usage: runner.Usage, review: runner.Review, prepared: map[string]*cliPrepared{}}, nil
}

// A successful preflight can be held by admission before Dispatch. Expire only
// its short-lived in-memory permit context; source/artifact state is retained.
func (d *CLIDriver) purgePrepared() {
	now := time.Now()
	for id, prepared := range d.prepared {
		if prepared != nil && !now.Before(prepared.expires) {
			delete(d.prepared, id)
		}
	}
}

func (d *CLIDriver) Prepare(ctx context.Context, operation reviewexec.Operation, configuration reviewexec.Configuration, attempt reviewexec.Attempt) (preparation reviewexec.Preparation, failure error) {
	d.mu.Lock()
	d.purgePrepared()
	d.mu.Unlock()
	current, err := d.engine.ExecutionContext(ctx, operation.ProjectID, operation.ID)
	if err != nil {
		return preparation, err
	}
	if !sameOperation(operation, current.Operation) || !reflect.DeepEqual(configuration, current.Configuration) || configuration.CapabilityID != d.capability.ID || operation.Channel != "cli" || attempt.Provider != "coderabbit" || attempt.Account != d.capability.Account || attempt.ProjectID != operation.ProjectID || attempt.OperationID != operation.ID || attempt.ID != current.Operation.AttemptID || attempt.State != "preparing" || attempt.Revision != 1 {
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
	reads := newSourceReads(d.reader)
	plan, _, err := resolveReviewSource(ctx, d.engine, reads, current.RepositoryID, current.Configuration, current.Operation)
	if err != nil {
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
	usage, err := d.usage(ctx, workspace, plan.Repository.FullName, d.capability.Organization)
	if err != nil {
		return preparation, err
	}
	if err := d.engine.RecordCLIAllowance(ctx, operation.ProjectID, attempt.ID, attempt.Revision, reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Remaining: usage.Remaining, Limit: usage.Limit, FullCapacityInMS: usage.FullCapacityInMS, RollingWindowMS: usage.RollingWindowMS, ChangedFiles: int64(len(paths))}); err != nil {
		return preparation, err
	}
	// Only absence permits fresh metadata fallback; an actual projection
	// conflict or storage failure must not be disguised as a missing row.
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
	d.mu.Lock()
	d.prepared[attempt.ID] = &cliPrepared{operation: current.Operation, configuration: current.Configuration, plan: plan, workspace: workspace, anchor: anchor, expires: time.Now().Add(60 * time.Second)}
	d.mu.Unlock()
	return reviewexec.Preparation{Anchor: anchor}, nil
}

func (d *CLIDriver) Dispatch(ctx context.Context, operation reviewexec.Operation, attempt reviewexec.Attempt, _ func(context.Context, string) error) (reviewexec.Result, error) {
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
	if !sameOperation(operation, prepared.operation) || attempt.ProjectID != operation.ProjectID || attempt.OperationID != operation.ID || attempt.Provider != "coderabbit" || attempt.Account != d.capability.Account || attempt.State != "active" || attempt.Anchor == nil || *attempt.Anchor != prepared.anchor {
		return failed("execution_binding_changed")
	}
	reads := newSourceReads(d.reader)
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
	if err := d.engine.VerifyAction(ctx, operation.ProjectID, attempt.ID, attempt.Revision, predecessors...); err != nil {
		return failed("admission_changed")
	}
	started := time.Now().UTC()
	capture, executionErr := d.review(ctx, prepared.workspace)
	finished := time.Now().UTC()
	possibleStart := capture.Started
	var processFailure *reviewprocess.Error
	if errors.As(executionErr, &processFailure) && processFailure.OutcomeUnknown {
		possibleStart = true
	}
	// Local capture retention remains finite but survives caller cancellation.
	retentionCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	retained, err := RetainCLI(retentionCtx, d.captures, d.engine, d.manager, attempt, prepared.workspace, capture, started, finished)
	if err == nil {
		_, err = d.engine.PublishEvidence(retentionCtx, operation.ProjectID, retained.Receipt.ID, nil)
		// Authoritative retained evidence remains usable when current browser
		// metadata disappeared. Other publication failures remain conservative.
		var missing *core.Error
		if errors.As(err, &missing) && missing.Code == "review_projection_missing" {
			err = nil
		}
	}
	if err != nil {
		state := "failed"
		if possibleStart {
			state = "uncertain"
		}
		return reviewexec.Result{State: state, Reason: "capture_retention_failed"}, nil
	}
	if executionErr != nil {
		if possibleStart {
			return reviewexec.Result{State: "uncertain", Reason: "execution_interrupted"}, nil
		}
		return failed("process_not_started")
	}
	if !capture.Started {
		return failed("process_not_started")
	}
	if retained.Complete && retained.Outcome == "completed" {
		return reviewexec.Result{State: "completed", Reason: "capture_retained"}, nil
	}
	switch retained.Outcome {
	case "failed", "cancelled", "rate_limited":
		return reviewexec.Result{State: retained.Outcome, Reason: "capture_retained"}, nil
	}
	return reviewexec.Result{State: "uncertain", Reason: retained.Reason}, nil
}
