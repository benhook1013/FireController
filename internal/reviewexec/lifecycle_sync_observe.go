package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func (e *Engine) observeLifecycle(ctx context.Context, target lifecycleTarget, s *lifecycleSchedule) error {
	state, head := "", ""
	var providerErr error
	ciCollected := false
	published := false
	defer func() { s.finish(target, state, head, ciCollected, published, providerErr) }()
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
	repo, err := target.reader.Repository(readCtx, target.repository)
	if err != nil {
		cancel()
		providerErr = err
		return nil
	}
	if repo.ID != target.repository || !repository.MatchString(repo.FullName) {
		cancel()
		providerErr = invalid()
		return nil
	}
	pull, err := target.reader.Pull(readCtx, repo.FullName, target.number)
	cancel()
	if err != nil {
		providerErr = err
		return nil
	}
	if !sameTarget(pull, target.repository, target.number) || pull.State != "open" && pull.State != "closed" || pull.Merged && pull.State != "closed" || pull.ChangedFiles < 0 || strings.TrimSpace(pull.Title) == "" {
		providerErr = invalid()
		return nil
	}
	plan := SourcePlan{Repository: repo, Pull: pull, ObservedAt: stamp()}
	display := sourceDisplayPull(plan)
	state, head = display.State, pull.HeadSHA
	mergeability := pull.Mergeability
	if mergeability != "mergeable" && mergeability != "conflicting" {
		mergeability = "unknown"
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
				_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: target.project, PrincipalID: "service-lifecycle-sync"}, target.repository, pins, []reviews.SourceStatus{{Pull: display, Mergeability: mergeability}})
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
		bound, err = e.lifecycleEnrichmentCurrent(ctx, tx, target, lifecycleComparison{head, pull.BaseSHA, pull.BaseRef, state})
		return err
	})
	if err != nil || !bound {
		return err
	}
	ciState := ""
	ciReason := ""
	// CI is less frequent than lifecycle reads. Same-comparison retained status
	// is not refreshed by a cheap poll; a new head always requires fresh evidence.
	collectCI := state == "open" && (target.due.ciHead != head || target.due.ciBaseSHA != pull.BaseSHA || target.due.ciBaseRef != pull.BaseRef || !s.options.Now().Before(target.due.ci))
	if collectCI {
		ciCollected = true
		ciState = "unknown"
		ciReason = "ci_reader_unavailable"
		if reader, ok := target.reader.(sourceCIReader); ok {
			ciCtx, ciCancel := context.WithTimeout(ctx, 30*time.Second)
			observation, readErr := reader.ObserveRequiredCI(ciCtx, sourceCITarget(sourcePlanObservation(plan)))
			ciCancel()
			if readErr != nil {
				providerErr = readErr
				ciReason = lifecycleErrorCode(readErr)
			} else if validSourceCI(observation, sourcePlanObservation(plan)) && !observation.StartedAt.Before(mustLifecycleTime(plan.ObservedAt)) && !observation.ObservedAt.Before(observation.StartedAt) && !observation.ObservedAt.After(time.Now().Add(5*time.Second)) {
				ciReason = observation.Reason
				if ciReason == "" && observation.State == "unproven" {
					ciReason = "ci_unproven"
				}
				switch observation.State {
				case "satisfied":
					ciState = "success"
				case "failed":
					ciState = "failure"
				case "pending":
					ciState = "pending"
				}
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	external, hostedCollected, externalErr := collectExternalHosted(ctx, target, repo.FullName, providerErr)
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
			bound, err = e.lifecycleEnrichmentCurrent(ctx, tx, target, lifecycleComparison{head, pull.BaseSHA, pull.BaseRef, state})
			if err != nil || !bound {
				return err
			}
			_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: target.project, PrincipalID: "service-lifecycle-sync"}, target.repository, target.pins, []reviews.SourceStatus{{Pull: display, CIState: ciState, Mergeability: mergeability}})
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
		if hostedCollected {
			target.due.hostedCheckedAt = s.options.Now()
			target.due.hostedError = lifecycleErrorCode(externalErr)
			if externalErr == nil && externalDeferred {
				target.due.hostedError = lifecycleErrorCode(externalHostedUnproven())
			}
		}
	}
	return err
}

func (e *Engine) lifecycleEnrichmentCurrent(ctx context.Context, tx *sql.Tx, target lifecycleTarget, comparison lifecycleComparison) (bool, error) {
	current, err := readLifecycleComparison(ctx, tx, target.project, target.number)
	if err != nil || current != comparison {
		return false, err
	}
	pins, err := reviews.PinSourceMetadata(ctx, tx, target.project, []int64{target.number})
	return err == nil && len(pins) == 1 && pins[target.number] == target.pins[target.number], err
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
	_, revision, err := readConfiguration(ctx, tx, target.project)
	if err != nil {
		return false, err
	}
	c, err := sourceConfiguration(ctx, tx, target.project, revision)
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
