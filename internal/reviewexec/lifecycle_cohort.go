package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type lifecycleCohortFactory interface {
	NewCIGateCohort(context.Context, []reviewprovider.CITarget, string) (*reviewprovider.CIGateCohort, error)
}

type lifecyclePrepared struct {
	pull       reviewprovider.Pull
	repo       reviewprovider.Repository
	observedAt string
	err        error
}

// Only unchanged, already-due periodic work may join. Hints and source movement
// retain their individual admission and cannot become late cohort subscribers.
func periodicCohortDue(d lifecycleDue, now time.Time) bool {
	h := d.hints[lifecycleEnrichment]
	return d.head != "" && d.head == d.enrichmentHead && d.baseSHA == d.enrichmentBaseSHA && d.baseRef == d.enrichmentBaseRef && h.wanted == h.done && !now.Before(d.enrichmentNext) && !now.Before(d.ci)
}

// A two-member cohort holds the existing single account/project reservation.
// It does not add provider concurrency. All fresh source plans precede the
// shared authority read; original per-target publication guards remain intact.
func (e *Engine) observeLifecycleCohort(ctx context.Context, first lifecycleTarget, readers map[string]TargetReader, s *lifecycleSchedule) error {
	if s.options.BatchCIWitnesses {
		defer func() { s.batching.report(s.options.Now()) }()
	}
	factory, ok := first.reader.(lifecycleCohortFactory)
	if first.lane != lifecycleEnrichment || first.watch != nil || first.forceCI || first.ciOnly || first.comparison.state != "open" || !periodicCohortDue(first.due, s.options.Now()) || !ok {
		return e.observeLifecycle(ctx, first, s)
	}
	if support, known := first.reader.(interface{ SupportsCIGateCohort() bool }); known && !support.SupportsCIGateCohort() {
		return e.observeLifecycle(ctx, first, s)
	}
	next, err := e.claimLifecycleMember(ctx, readers, s, lifecycleEnrichment, &first)
	if err != nil {
		s.finish(first, "", "", false, false, nil)
		return err
	}
	if next == nil {
		return e.observeLifecycle(ctx, first, s)
	}
	targets := []lifecycleTarget{first, *next}
	for i := range targets {
		targets[i].cohortOwned = true
	}
	defer func() {
		s.mu.Lock()
		delete(s.lanes[first.lane].projects, first.project)
		delete(s.lanes[first.lane].accounts, first.account)
		s.mu.Unlock()
	}()
	// Preparation is bounded separately; it performs no CI collection and cannot
	// publish or grant freshness. Changed bindings return to ordinary scheduling.
	prepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for i := range targets {
		t := &targets[i]
		var bound bool
		err := e.readAdmission(prepCtx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			bound, err = e.lifecycleBinding(ctx, tx, *t)
			return err
		})
		if err != nil {
			return err
		}
		if !bound {
			return nil
		}
		readCtx, readCancel := context.WithTimeout(prepCtx, 15*time.Second)
		readCtx = reviewprovider.WithGitHubTarget(reviewprovider.WithGitHubCaller(readCtx, "lifecycle_enrichment_periodic"), t.repository, t.number)
		pull, repo, readErr := readLifecyclePull(readCtx, *t)
		readCancel()
		t.prepared = &lifecyclePrepared{pull: pull, repo: repo, err: readErr, observedAt: stamp()}
		if readErr != nil {
			// Publish the ordinary failure/backoff once; do not read another member.
			return e.observeLifecycle(ctx, *t, s)
		}
	}
	members := make([]reviewprovider.CITarget, 0, len(targets))
	gate := targets[0].prepared.repo.DefaultBranch
	eligible := true
	for _, t := range targets {
		p := t.prepared
		if p.pull.State != "open" || p.pull.Merged || p.repo.DefaultBranch != gate || p.repo.ID != first.repository || p.repo.FullName != targets[0].prepared.repo.FullName || p.pull.HeadSHA != t.comparison.head || p.pull.BaseSHA != t.comparison.baseSHA || p.pull.BaseRef != t.comparison.baseRef {
			eligible = false
		}
		members = append(members, sourceCITarget(sourcePlanObservation(SourcePlan{Repository: p.repo, Pull: p.pull, ObservedAt: p.observedAt})))
	}
	var cohortCtx context.Context
	if eligible {
		bounded, closeCohort := context.WithTimeout(ctx, reviewprovider.CIObservationTimeout)
		defer closeCohort()
		cohort, err := factory.NewCIGateCohort(bounded, members, gate)
		if err == nil {
			defer cohort.Close()
			batch := s.options.BatchCIWitnesses && !lifecycleCohortHasSubscribers(s, targets)
			if s.options.BatchCIWitnesses {
				s.batching.record(s.options.Now(), batchEligible)
				if !batch {
					s.batching.record(s.options.Now(), batchWatchFallback)
				}
			}
			if batch {
				batchCtx := reviewprovider.WithGitHubCaller(bounded, "lifecycle_enrichment_periodic")
				if err := cohort.PrepareBatched(batchCtx); err != nil {
					var accountFailure *reviewprovider.CIBatchAccountFailure
					if errors.As(err, &accountFailure) {
						// The batch stops before publication. Apply its account-wide
						// backoff without assigning a partner's failure to this PR.
						s.mu.Lock()
						s.finishAccountLocked(targets[0], accountFailure.Unwrap(), s.options.Now())
						s.mu.Unlock()
						s.batching.record(s.options.Now(), batchAccountAbort)
						return nil
					}
					s.batching.record(s.options.Now(), batchPrepareRefused)
					return err
				}
				s.batching.record(s.options.Now(), batchPrepared)
			}
			cohortCtx = bounded
			for i := range targets {
				targets[i].cohort = cohort
				targets[i].ciPreparedBatch = batch
			}
		}
	}
	for _, t := range targets {
		s.mu.Lock()
		cooling := s.options.Now().Before(s.accounts[t.account].until)
		s.mu.Unlock()
		if ctx.Err() != nil || cooling || cohortCtx != nil && cohortCtx.Err() != nil {
			return nil
		}
		if err := e.observeLifecycle(ctx, t, s); err != nil {
			return err
		}
	}
	return nil
}

// Prefer the existing fresh lifecycle/watch sharing path for already subscribed
// targets. This is only an optimization refusal; it grants no watch authority.
// A new subscriber after this snapshot still requires post-registration reads.
func lifecycleCohortHasSubscribers(s *lifecycleSchedule, targets []lifecycleTarget) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.watches {
		if entry == nil || len(entry.owners) == 0 {
			continue
		}
		for _, target := range targets {
			if key.binding.project == target.project && key.number == target.number {
				return true
			}
		}
	}
	return false
}
