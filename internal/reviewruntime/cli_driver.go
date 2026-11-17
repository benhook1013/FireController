package reviewruntime

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// CLIDriver composes one trusted operator capability. Runtime ownership of its
// concrete manager, runner, engine and store is established by the caller.
type CLIDriver struct {
	store        *core.Store
	engine       *reviewexec.Engine
	capability   OperatorCapability
	manager      *reviewworkspace.Manager
	captures     *CaptureStore
	exhaustions  *CLIExhaustionStore
	reader       reviewexec.TargetReader
	fetch        func(context.Context, int64, string, string, string, string) error
	verify       func(context.Context, reviewworkspace.Workspace) error
	usage        func(context.Context, reviewworkspace.Workspace, string, string) (reviewprocess.Usage, error)
	review       func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error)
	mu           sync.Mutex
	prepared     map[string]*cliPrepared
	recovering   atomic.Bool
	recoveryWait func(context.Context, time.Duration) error
}

func (*CLIDriver) String() string     { return "CLIDriver{private}" }
func (d *CLIDriver) GoString() string { return d.String() }

type cliStartCallbackKey struct{}

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
	exhaustions, err := NewCLIExhaustionStore(filepath.Join(filepath.Dir(captures.root), "cli-exhaustion-captures"))
	if err != nil {
		return nil, err
	}
	return &CLIDriver{store: store, engine: engine, capability: capability, manager: manager, captures: captures, exhaustions: exhaustions, reader: reader, fetch: manager.Fetch, verify: manager.Verify, usage: runner.Usage, review: func(ctx context.Context, workspace reviewworkspace.Workspace) (reviewprocess.Capture, error) {
		onStart, _ := ctx.Value(cliStartCallbackKey{}).(func(time.Time))
		return runner.ReviewWithStart(ctx, workspace, onStart)
	}, prepared: map[string]*cliPrepared{}}, nil
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
		return preparation, reviewexec.PreparationFailureAt("execution_context", err)
	}
	ctx = reviewprovider.WithGitHubCaller(ctx, "cli_preparation")
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
	plan, err := retryCLIPreparationSource(ctx, operation.ID, "source_resolution", func() (reviewexec.SourcePlan, error) {
		plan, _, err := resolvePreparingIntentSource(ctx, d.engine, reads, current.RepositoryID, current.Configuration, current.Operation)
		return plan, err
	})
	if err != nil {
		return preparation, reviewexec.PreparationFailureAt("source_resolution", err)
	}
	workspace, changedFiles, err := prepareReviewWorkspace(ctx, d.fetch, d.verify, d.manager, d.capability.GitHubCredential(), current, plan, attempt.ID)
	if err != nil {
		return preparation, reviewexec.PreparationFailureAt("workspace", err)
	}
	anchor := reviewexec.Anchor{RepositoryID: current.RepositoryID, Repository: plan.Repository.FullName, PullNumber: plan.Pull.Number, CandidateSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, ParentSHA: plan.ParentSHA, ParentPullNumber: plan.ParentPullNumber, BaseRef: plan.Pull.BaseRef, MergeBaseSHA: workspace.MergeBaseSHA, PatchSHA256: workspace.PatchSHA256, ObservedAt: plan.ObservedAt}
	baseline, err := d.engine.RetryBaseline(ctx, operation.ProjectID, operation.ID)
	if err != nil {
		if retryableHostedRetention(err) || ctx.Err() != nil {
			return preparation, &reviewexec.PreparationRecoveryError{Err: err}
		}
		return preparation, reviewexec.PreparationFailureAt("retry_baseline", err)
	}
	if baseline != nil && !sameHostedRetryComparison(*baseline, anchor) {
		return preparation, reviewHold("source_changed")
	}
	usage, err := d.usage(ctx, workspace, plan.Repository.FullName, d.capability.Organization)
	if err != nil {
		var failure *reviewprocess.Error
		if errors.As(err, &failure) {
			if failure.Code == "rate_limited" {
				if usage.Remaining != 0 {
					return preparation, reviewHold("allowance_unavailable")
				}
				// A zero observation cannot authorize dispatch. Preserve an intervening
				// local refusal before classifying this as another provider wait.
				observation := reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Remaining: usage.Remaining, Limit: usage.Limit, FullCapacityInMS: usage.FullCapacityInMS, RollingWindowMS: usage.RollingWindowMS, ChangedFiles: changedFiles}
				retentionCtx, retentionCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				retainErr := d.exhaustions.Save(retentionCtx, reviewexec.CLIExhaustionRecovery{Operation: current.Operation, Attempt: attempt, Observation: observation})
				retentionCancel()
				if retainErr != nil {
					return preparation, &reviewexec.PreparationRecoveryError{Err: retainErr}
				}
				if err := retryAllowanceRetention(ctx, waitPreparation, func() error {
					return core.NormalizeStorageError(d.engine.RecordCLIExhaustion(ctx, operation.ProjectID, attempt.ID, attempt.Revision, observation))
				}); err != nil {
					if retryableHostedRetention(err) || ctx.Err() != nil {
						return preparation, &reviewexec.PreparationRecoveryError{Err: err}
					}
					var refused *core.Error
					if errors.As(err, &refused) && (refused.Status == 409 || refused.Status == 403) {
						return preparation, err
					}
					return preparation, reviewHold("allowance_unavailable")
				}
				return preparation, reviewHold("allowance_exhausted")
			}
			if failure.Code == "usage_unavailable" {
				switch failure.FailureStage {
				case reviewprocess.FailureStageCommandNotStarted:
					return preparation, reviewHold("allowance_command_not_started")
				case reviewprocess.FailureStageTimeout:
					return preparation, reviewHold("allowance_timeout")
				case reviewprocess.FailureStageCommandFailed:
					return preparation, reviewHold("allowance_command_failed")
				case reviewprocess.FailureStageResponseUnproven:
					return preparation, reviewHold("allowance_response_unproven")
				case reviewprocess.FailureStageLookupFailed:
					return preparation, reviewHold("allowance_provider_lookup_failed")
				}
			}
		}
		return preparation, reviewHold("allowance_unavailable")
	}
	allowanceObservedAt := time.Now().UTC()
	plan, err = retryCLIPreparationSource(ctx, operation.ID, "source_refresh", func() (reviewexec.SourcePlan, error) {
		return refreshAgedMaterializedSource(ctx, d.reader, current.RepositoryID, current.Configuration, current.Operation, plan, allowanceObservedAt)
	})
	if err != nil {
		return preparation, reviewexec.PreparationFailureAt("source_refresh", err)
	}
	anchor.ObservedAt = plan.ObservedAt
	if err := d.engine.RecordCLIAllowance(ctx, operation.ProjectID, attempt.ID, attempt.Revision, reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: allowanceObservedAt.Format(time.RFC3339Nano), Remaining: usage.Remaining, Limit: usage.Limit, FullCapacityInMS: usage.FullCapacityInMS, RollingWindowMS: usage.RollingWindowMS, ChangedFiles: changedFiles}); err != nil {
		return preparation, reviewexec.PreparationFailureAt("allowance_recording", err)
	}
	if err := publishPreparedAttempt(ctx, d.engine, operation.ProjectID, attempt.ID, plan); err != nil {
		return preparation, reviewexec.PreparationFailureAt("attempt_publication", err)
	}
	d.mu.Lock()
	d.prepared[attempt.ID] = &cliPrepared{operation: current.Operation, configuration: current.Configuration, plan: plan, workspace: workspace, anchor: anchor, expires: time.Now().Add(60 * time.Second)}
	d.mu.Unlock()
	return reviewexec.Preparation{Anchor: anchor}, nil
}

// Retry only read-only provider work. A refresh retry keeps the existing
// workspace and allowance observation and still checks the exact prepared source.
func retryCLIPreparationSource(ctx context.Context, operationID, stage string, read func() (reviewexec.SourcePlan, error)) (reviewexec.SourcePlan, error) {
	plan, err := read()
	var failure *reviewprovider.Error
	if ctx.Err() != nil || !errors.As(err, &failure) || !failure.RetryableSourceRead() {
		return plan, err
	}
	slog.InfoContext(ctx, "Retrying transient CLI preparation source read", "operation_id", operationID, "stage", stage, "provider_status", failure.Status)
	if err := waitPreparation(ctx, 5*time.Second); err != nil {
		return reviewexec.SourcePlan{}, err
	}
	return read()
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
		return failed(actionRefusalReason(err, "source_changed"))
	}
	_, predecessors, err := resolveReviewSource(ctx, d.engine, reads, prepared.anchor.RepositoryID, prepared.configuration, operation)
	if err != nil {
		return failed(actionRefusalReason(err, "source_admission_changed"))
	}
	if err := d.verify(ctx, prepared.workspace); err != nil {
		return failed("workspace_changed")
	}
	if err := d.engine.VerifyAction(ctx, operation.ProjectID, attempt.ID, attempt.Revision, predecessors...); err != nil {
		return failed(actionRefusalReason(err, "admission_changed"))
	}
	started := time.Now().UTC()
	onStart := func(at time.Time) {
		// Never retry or cancel an already spawned process if diagnostic
		// retention fails. The browser will correctly show unknown evidence.
		_ = d.engine.RecordActivityEvidence(ctx, operation.ProjectID, attempt.ID, reviewexec.ActivityEvidence{AttemptID: attempt.ID, HeadSHA: operation.HeadSHA, Kind: "cli_process_spawned", ObservedAt: at.Format(time.RFC3339Nano), ProcessStartedAt: at.Format(time.RFC3339Nano)})
	}
	capture, executionErr := d.review(context.WithValue(ctx, cliStartCallbackKey{}, onStart), prepared.workspace)
	finished := time.Now().UTC()
	possibleStart := capture.Started
	var processFailure *reviewprocess.Error
	if errors.As(executionErr, &processFailure) && processFailure.OutcomeUnknown {
		possibleStart = true
	}
	// Local capture retention remains finite but survives caller cancellation.
	retentionCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	retained, err := RetainCLI(retentionCtx, d.captures, d.engine, d.manager, attempt, prepared.workspace, capture, started, finished)
	cancel()
	failureReason := "capture_retention_failed"
	if err == nil {
		// Retention/Git verification must not consume publication's own bounded
		// local budget. A later observer can finish from this exact final ledger.
		publicationCtx, cancelPublication := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = d.engine.PublishEvidence(publicationCtx, operation.ProjectID, retained.Receipt.ID, nil)
		cancelPublication()
		failureReason = "capture_publication_pending"
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
		return reviewexec.Result{State: state, Reason: failureReason}, nil
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
		if retained.Outcome == "failed" && retained.Reason == "coverage_incomplete" {
			return reviewexec.Result{State: "failed", Reason: retained.Reason}, nil
		}
		return reviewexec.Result{State: retained.Outcome, Reason: "capture_retained"}, nil
	}
	return reviewexec.Result{State: "uncertain", Reason: retained.Reason}, nil
}
