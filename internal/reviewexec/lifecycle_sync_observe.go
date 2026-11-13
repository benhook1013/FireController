package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func (e *Engine) observeLifecycle(ctx context.Context, target lifecycleTarget, s *lifecycleSchedule) (result error) {
	if target.watch != nil {
		return e.observeCIWatch(ctx, target, s)
	}
	if target.lane == lifecycleMetadata {
		ctx = reviewprovider.WithGitHubCaller(ctx, "lifecycle_metadata")
	} else {
		ctx = reviewprovider.WithGitHubCaller(ctx, "lifecycle_enrichment")
	}
	state, head := "", ""
	var providerErr error
	ciCollected := false
	published := false
	defer func() {
		if result != nil {
			// No health success may survive a rolled-back write. Isolate only
			// errors explicitly attributed to this project's configuration or
			// this PR's display metadata, never arbitrary history corruption.
			published = false
		}
		s.finish(target, state, head, ciCollected, published, providerErr)
		if ctx.Err() == nil && lifecycleStoredStateCode(result, target.project, target.number) != "" {
			s.holdStoredState(target.project, target.number, target.lane, target.wakeComparison, result)
			result = nil
		}
	}()
	// Recheck selected configuration before provider work. A pause-only change
	// is intentionally harmless; changing repository, enrollment or capability holds.
	var bound bool
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		bound, err = e.lifecycleBinding(ctx, tx, target)
		return err
	})
	if err != nil || !bound {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	pull, repo, err := readLifecyclePull(readCtx, target)
	cancel()
	if err != nil {
		providerErr = err
		return nil
	}
	if !sameTarget(pull, target.repository, target.number) || pull.State != "open" && pull.State != "closed" || pull.Merged && pull.State != "closed" || pull.ChangedFiles < 0 || strings.TrimSpace(pull.Title) == "" {
		providerErr = &reviewprovider.Error{Code: "provider_invalid_response"}
		return nil
	}
	plan := SourcePlan{Repository: repo, Pull: pull, ObservedAt: stamp()}
	display := sourceDisplayPull(plan)
	// Reject invalid display fields before claiming a current comparison or
	// doing enrichment. The publisher can omit invalid records in a batch,
	// but a skipped PR must never count as a successful lifecycle observation.
	if !reviews.ValidSourceDisplayMetadata(display) {
		providerErr = &reviewprovider.Error{Code: "provider_invalid_response"}
		return nil
	}
	state, head = display.State, pull.HeadSHA
	mergeability := pull.Mergeability
	if mergeability != "mergeable" && mergeability != "conflicting" {
		mergeability = "unknown"
	}
	mergeState := pull.MergeStateStatus
	switch mergeState {
	case "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE":
	default:
		mergeState = "UNKNOWN"
	}
	target.due.baseSHA, target.due.baseRef = pull.BaseSHA, pull.BaseRef
	if target.lane == lifecycleMetadata {
		err = retrySourceRefreshLocal(ctx, func() error {
			published = false
			return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
				bound, err := e.lifecycleBinding(ctx, tx, target)
				if err != nil || !bound {
					return err
				}
				current, err := readLifecycleComparison(ctx, tx, target.project, target.number)
				if err != nil || current != target.comparison {
					return err
				}
				// Enrichment may update CI while this cheap provider read runs.
				// Re-pin that unchanged comparison instead of losing this poll.
				pins, err := reviews.PinSourceMetadata(ctx, tx, target.project, []int64{target.number})
				if err != nil {
					return err
				}
				_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: target.project, PrincipalID: "service-lifecycle-sync"}, target.repository, pins, []reviews.SourceStatus{{Pull: display, Mergeability: mergeability, MergeStateStatus: mergeState}})
				published = err == nil
				return err
			})
		})
		if err == nil && published {
			target.due.successAt = s.options.Now()
		}
		return err
	}
	// A fresh enrichment read is not permission to move the stored comparison.
	// Let the independent metadata lane publish movement first, then collect
	// expensive evidence for exactly that retained comparison.
	err = e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		bound, err = e.lifecycleEnrichmentCurrent(ctx, tx, &target, lifecycleComparison{head, pull.BaseSHA, pull.BaseRef, state})
		return err
	})
	if err != nil || !bound {
		return err
	}
	ciState := ""
	ciReason := ""
	// CI is less frequent than lifecycle reads. Same-comparison retained status
	// is not refreshed by a cheap poll; a new head always requires fresh evidence.
	collectCI := state == "open" && (target.forceCI || target.due.ciHead != head || target.due.ciBaseSHA != pull.BaseSHA || target.due.ciBaseRef != pull.BaseRef || !s.options.Now().Before(target.due.ci))
	if collectCI {
		target.due.ciCommit = ""
		ciCollected = true
		ciState = "unknown"
		ciReason = "ci_reader_unavailable"
		target.due.ciGateRef, target.due.ciGateState = "", ""
		ciCtx, ciCancel := context.WithTimeout(ctx, reviewprovider.CIObservationTimeout)
		observation, reason, readErr := observeLifecycleCIGate(ciCtx, target.reader, plan)
		if readErr != nil {
			providerErr = readErr
			ciReason = sourceCIErrorReason(ciCtx, readErr)
		} else if reason != "" {
			ciReason = reason
		} else {
			target.due.ciGateRef = repo.DefaultBranch
			target.due.ciCommit = observation.SelectedCommitSHA
			ciReason = observation.Reason
			ciState = sourceCIQueueState(observation.State)
			target.due.ciGateState = ciState
		}
		ciCancel()
	}
	if ctx.Err() != nil {
		return nil
	}
	var external []reviewprovider.ExternalHostedRun
	var hostedCollected bool
	var externalErr error
	// Keep catching up incomplete inventory and check a new comparison
	// immediately. Only repeat a healthy scan on the slower fallback cadence.
	observedComparison := lifecycleComparison{head, pull.BaseSHA, pull.BaseRef, state}
	hostedDue := observedComparison != target.due.hostedComparison || target.due.hostedError != "" || !s.options.Now().Before(target.due.hostedCheckedAt.Add(healthyHostedInterval))
	if !target.ciOnly && hostedDue {
		external, hostedCollected, externalErr = collectExternalHosted(ctx, target, repo.FullName, providerErr)
	}
	if externalErr != nil && (providerErr == nil || lifecycleProviderDelay(externalErr, 0) > lifecycleProviderDelay(providerErr, 0)) {
		providerErr = externalErr
	}
	if ctx.Err() != nil {
		return nil
	}
	externalDeferred := false
	collectionErr := providerErr
	err = retryLifecyclePublication(ctx, func(appendLimit int) error {
		externalDeferred = false
		providerErr = collectionErr
		published = false
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			bound, err := e.lifecycleBinding(ctx, tx, target)
			if err != nil || !bound {
				return err
			}
			bound, err = e.lifecycleEnrichmentCurrent(ctx, tx, &target, lifecycleComparison{head, pull.BaseSHA, pull.BaseRef, state})
			if err != nil || !bound {
				return err
			}
			_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: target.project, PrincipalID: "service-lifecycle-sync"}, target.repository, target.pins, []reviews.SourceStatus{{Pull: display, CIState: ciState, Mergeability: mergeability, MergeStateStatus: mergeState}})
			if err != nil {
				return err
			}
			published = true
			if externalErr == nil && len(external) > 0 {
				if appendLimit == 0 {
					externalDeferred = true
					if providerErr == nil {
						providerErr = externalHostedUnproven()
					}
					return nil
				}
				runs, held, err := e.externalHostedInputs(ctx, tx, target, external, appendLimit)
				if err != nil {
					return err
				}
				if held && providerErr == nil {
					providerErr = externalHostedUnproven()
				}
				externalDeferred = held
				if len(runs) > 0 {
					unsupported, err := publishExternalHostedInputsBounded(ctx, tx, target, runs, appendLimit)
					if unsupported && providerErr == nil {
						providerErr = externalHostedUnproven()
					}
					externalDeferred = externalDeferred || unsupported
					return err
				}
			}
			return nil
		})
	})
	if err == nil && published {
		if collectCI {
			target.due.ciCheckedAt = s.options.Now()
			target.due.ciError = ciReason
		}
		if target.hostedRefusal != "" {
			target.due.hostedCheckedAt = s.options.Now()
			target.due.hostedError = target.hostedRefusal
		}
		if hostedCollected {
			target.due.hostedCheckedAt = s.options.Now()
			target.due.hostedComparison = observedComparison
			target.due.hostedError = lifecycleErrorCode(externalErr)
			if externalErr == nil && externalDeferred {
				target.due.hostedError = lifecycleErrorCode(externalHostedUnproven())
			}
		}
	}
	return err
}

// Try the configured name first, but trust only the ID in the provider's
// response. A renamed repository can be recovered through its stable ID.
func readLifecyclePull(ctx context.Context, target lifecycleTarget) (reviewprovider.Pull, reviewprovider.Repository, error) {
	if reader, ok := target.reader.(interface {
		PullWithRepository(context.Context, string, int64) (reviewprovider.Pull, reviewprovider.Repository, error)
	}); ok && repository.MatchString(target.repositoryName) {
		pull, repo, err := reader.PullWithRepository(ctx, target.repositoryName, target.number)
		if err == nil && repo.ID == target.repository && repository.MatchString(repo.FullName) {
			return pull, repo, nil
		}
		var failure *reviewprovider.Error
		if err != nil && !errors.Is(err, reviewprovider.ErrCombinedPullUnsupported) &&
			(!errors.As(err, &failure) || (failure.Code != "provider_not_found" && failure.Status != 301 && failure.Status != 302 && failure.Status != 307 && failure.Status != 308)) {
			return reviewprovider.Pull{}, reviewprovider.Repository{}, err
		}
	}
	repo, err := target.reader.Repository(ctx, target.repository)
	if err != nil {
		return reviewprovider.Pull{}, reviewprovider.Repository{}, err
	}
	if repo.ID != target.repository || !repository.MatchString(repo.FullName) {
		return reviewprovider.Pull{}, reviewprovider.Repository{}, &reviewprovider.Error{Code: "provider_invalid_response"}
	}
	pull, err := target.reader.Pull(ctx, repo.FullName, target.number)
	return pull, repo, err
}

func (e *Engine) lifecycleEnrichmentCurrent(ctx context.Context, tx *sql.Tx, target *lifecycleTarget, comparison lifecycleComparison) (bool, error) {
	current, err := readLifecycleComparison(ctx, tx, target.project, target.number)
	if err != nil || current != comparison {
		return false, err
	}
	pins, err := reviews.PinSourceMetadata(ctx, tx, target.project, []int64{target.number})
	currentPin := err == nil && len(pins) == 1 && pins[target.number] == target.pins[target.number]
	// A metadata-only publication invalidates the pin, not this CI request.
	// Preserve its generation with the existing attempt time so retry remains
	// throttled and still collects CI before the ordinary CI interval expires.
	target.retainCIHint = err == nil && len(pins) == 1 && !currentPin && target.forceCI && comparison == target.comparison
	return currentPin, err
}

// A locally expensive batch must not monopolize a scheduler worker by retrying
// the same rolled-back transaction forever. Reduce the append budget after an
// overload, ultimately retaining lifecycle metadata alone. Deferred external
// results remain discoverable at the next tick. Never repeat provider reads.
func retryLifecyclePublication(ctx context.Context, publish func(int) error) error {
	limit := 100
	return retrySourceRefreshLocal(ctx, func() error {
		err := publish(limit)
		var failure *core.Error
		if limit > 0 && errors.As(err, &failure) && failure.Code == "overloaded" {
			limit /= 2
		}
		return err
	})
}

func mustLifecycleTime(value string) time.Time {
	instant, _ := time.Parse(time.RFC3339Nano, value)
	return instant
}

func (e *Engine) lifecycleBinding(ctx context.Context, tx *sql.Tx, target lifecycleTarget) (bool, error) {
	c, err := readLifecycleConfiguration(ctx, tx, target.project)
	if err != nil {
		return false, err
	}
	var repo int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, target.project).Scan(&repo); err != nil {
		return false, err
	}
	cap, ok := e.capabilities[c.CapabilityID]
	if !ok || repo != target.repository || cap.Fingerprint != target.fingerprint || !sameSourceConfiguration(c, target.configuration) {
		return false, nil
	}
	var visible bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls p WHERE p.project_id=? AND p.number=? AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number))`, target.project, target.number).Scan(&visible); err != nil {
		return false, err
	}
	return visible, nil
}

// Use the final branch's required-check authority without replacing the actual
// PR comparison. A stacked result never claims a hypothetical final merge ran.
func observeLifecycleCIGate(ctx context.Context, reader TargetReader, plan SourcePlan) (reviewprovider.CIObservation, string, error) {
	var observation reviewprovider.CIObservation
	gate := plan.Repository.DefaultBranch
	if gate == "" || !safeText(gate, 256) {
		return observation, "ci_gate_unproven", nil
	}
	source := sourcePlanObservation(plan)
	var err error
	if gateReader, ok := advertisedCIGateReader(reader); ok {
		observation, err = gateReader.ObserveCIGate(ctx, sourceCITarget(source), gate)
	} else if baseReader, ok := reader.(sourceCIReader); ok && gate == source.BaseRef {
		// Older adapters are safe only when selected and actual gates coincide.
		observation, err = baseReader.ObserveRequiredCI(ctx, sourceCITarget(source))
	} else {
		return observation, "ci_gate_reader_unavailable", nil
	}
	if err != nil {
		return reviewprovider.CIObservation{}, "", err
	}
	if !validCIWithAuthorityBranch(observation, source, gate, false) || observation.StartedAt.Before(mustLifecycleTime(source.ObservedAt)) || observation.ObservedAt.Before(observation.StartedAt) || observation.ObservedAt.After(time.Now().Add(5*time.Second)) {
		return reviewprovider.CIObservation{}, "ci_observation_unproven", nil
	}
	finalRepo, err := reader.Repository(ctx, plan.Repository.ID)
	if err != nil {
		return reviewprovider.CIObservation{}, "", err
	}
	if finalRepo.ID != plan.Repository.ID || finalRepo.FullName != plan.Repository.FullName || finalRepo.DefaultBranch != gate {
		return reviewprovider.CIObservation{}, "ci_gate_changed", nil
	}
	return observation, "", nil
}
