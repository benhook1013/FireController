package reviewexec

import (
	"context"
	"database/sql"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type metadataPairReader interface {
	PullMetadataPair(context.Context, int64, string, [2]int64) [2]reviewprovider.PullMetadataPairResult
}

func periodicMetadataDue(d lifecycleDue, now time.Time) bool {
	h := d.hints[lifecycleMetadata]
	return h.wanted == h.done && !now.Before(d.next)
}

// Pair only naturally due metadata from one existing account/project reservation.
// Events stay on their individual path. The ordinary publisher remains authority
// for local binding, source-pin and concurrent comparison checks.
func (e *Engine) observeMetadataCohort(ctx context.Context, first lifecycleTarget, readers map[string]TargetReader, s *lifecycleSchedule) error {
	reader, ok := first.reader.(metadataPairReader)
	if !ok || !periodicMetadataDue(first.due, s.options.Now()) || first.hintGeneration != 0 || !repository.MatchString(first.repositoryName) {
		return e.observeLifecycle(ctx, first, s)
	}
	if support, known := first.reader.(interface{ SupportsMetadataPair() bool }); known && !support.SupportsMetadataPair() {
		return e.observeLifecycle(ctx, first, s)
	}
	next, err := e.claimLifecycleMember(ctx, readers, s, lifecycleMetadata, &first)
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
		delete(s.lanes[lifecycleMetadata].projects, first.project)
		delete(s.lanes[lifecycleMetadata].accounts, first.account)
		s.mu.Unlock()
	}()
	for _, target := range targets {
		var bound bool
		err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			bound, err = e.lifecycleBinding(ctx, tx, target)
			return err
		})
		if err != nil {
			return err
		}
		if !bound {
			return nil
		}
	}
	rows := reader.PullMetadataPair(reviewprovider.WithGitHubCaller(ctx, "lifecycle_metadata"), first.repository, first.repositoryName, [2]int64{first.number, next.number})
	// A shared-query error cannot publish successful partner evidence as if the
	// account read completed. Preserve source evidence; invalid inventory uses
	// normal failed-observation cadence, account errors use ordinary cooldown.
	for _, row := range rows {
		if row.Err != nil {
			if lifecycleInventoryInconclusive(row.Err) {
				// Malformed evidence is not an account outage. Record an ordinary
				// failed observation for each member so it cannot hot-loop while
				// preserving source success and any later hint generation.
				for _, target := range targets {
					s.finish(target, "", "", false, false, row.Err)
				}
			} else {
				s.mu.Lock()
				s.finishAccountLocked(first, row.Err, s.options.Now())
				s.mu.Unlock()
			}
			return nil
		}
	}
	observedAt := stamp()
	for i := range targets {
		targets[i].prepared = &lifecyclePrepared{pull: rows[i].Pull, repo: rows[i].Repository, observedAt: observedAt}
		if err := e.observeLifecycle(ctx, targets[i], s); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}
