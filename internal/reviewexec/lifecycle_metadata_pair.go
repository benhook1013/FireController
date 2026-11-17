package reviewexec

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type metadataPairReader interface {
	PullMetadataPair(context.Context, int64, string, [2]int64) [2]reviewprovider.PullMetadataPairResult
}

type metadataPairQualifier interface {
	QualifyMetadataPair(context.Context, int64, string, [2]int64) (reviewprovider.MetadataPairQualification, error)
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
	var qualification *reviewprovider.MetadataPairQualification
	defer func() {
		if qualification != nil {
			slog.Info("metadata_pair_qualification", "bucket", qualification.Bucket, "state", qualification.State, "transport_calls", qualification.Requests)
		}
	}()
	qualifier, canQualify := first.reader.(metadataPairQualifier)
	if s.options.QualifyMetadataPair {
		if support, known := first.reader.(interface{ SupportsMetadataQualification() bool }); known && !support.SupportsMetadataQualification() {
			canQualify = false
		}
		s.mu.Lock()
		claimed := s.metadataQualificationClaimed[first.account]
		s.mu.Unlock()
		if !canQualify || claimed {
			return e.observeLifecycle(ctx, first, s)
		}
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
	if s.options.QualifyMetadataPair {
		qualify := false
		if !lifecycleCohortHasSubscribers(s, targets) {
			s.mu.Lock()
			if s.metadataQualificationClaimed == nil {
				s.metadataQualificationClaimed = map[string]bool{}
			}
			if !s.metadataQualificationClaimed[first.account] && !s.options.Now().Before(s.accounts[first.account].until) {
				s.metadataQualificationClaimed[first.account] = true
				qualify = true
			}
			s.mu.Unlock()
		}
		if qualify {
			result, probeErr := qualifier.QualifyMetadataPair(reviewprovider.WithGitHubCaller(ctx, "lifecycle_metadata"), first.repository, first.repositoryName, [2]int64{first.number, next.number})
			qualification = &result
			if probeErr != nil {
				s.mu.Lock()
				s.finishAccountLocked(targets[0], probeErr, s.options.Now())
				s.mu.Unlock()
				return nil
			}
		}
		// Diagnostic facts never become prepared source evidence. Both members
		// receive the ordinary fresh individual observation and publication guards.
		for _, target := range targets {
			s.mu.Lock()
			cooling := s.options.Now().Before(s.accounts[target.account].until)
			s.mu.Unlock()
			if ctx.Err() != nil || cooling {
				return nil
			}
			if err := e.observeLifecycle(ctx, target, s); err != nil {
				return err
			}
		}
		return nil
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
				s.finishAccountLocked(targets[0], row.Err, s.options.Now())
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
