package reviewexec

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Shared completion retains the same generation, cancellation and hint rules.
func (s *lifecycleSchedule) finishCIWatch(ctx context.Context, target lifecycleTarget, claim *ciWatchClaim, item CIWatchItem, providerErr error, releaseAccount bool) {
	outcome := shareDiscarded
	if claim.lifecycleShared {
		defer func() { s.sharing.record(s.options.Now(), outcome) }()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := claim.entry
	if ctx.Err() != nil {
		var limit *reviewprovider.Error
		if !errors.As(providerErr, &limit) || limit.RetryAfterSeconds <= 0 {
			providerErr = nil
		}
	}
	if releaseAccount {
		s.finishAccountLocked(target, providerErr, s.options.Now())
	}
	canceled := ctx.Err() != nil
	entry.cancel()
	entry.cancel, entry.inflight = nil, false
	// The selected test merge may not have been known when a status hint
	// arrived. Reconcile under the publication lock, including hints that
	// arrived during the final repository read. Overflow is conservative,
	// bounded refresh demand, never CI evidence.
	if canceled && len(entry.owners) > 0 && (len(entry.inflightSHAHints) > 0 || entry.inflightSHAOverflow) {
		// A hint may arrive after the last freshness-only owner cancels
		// this read. Without a completed comparison to reconcile it,
		// retain one bounded refresh for the remaining owners.
		entry.wanted++
	} else if !canceled && item.CICommitSHA != "" {
		refresh := entry.inflightSHAOverflow
		for _, head := range entry.inflightSHAHints {
			refresh = refresh || head == item.CICommitSHA
		}
		if refresh {
			entry.wanted++
		}
	}
	entry.inflightSHAHints, entry.inflightSHAOverflow = nil, false
	if canceled {
		// Cancellation releases capacity without replacing completed facts
		// or consuming demand from hints/subscribers that arrived meanwhile.
		return
	}
	entry.done, entry.version = claim.generation, claim.version
	entry.metadataDone = claim.metadataGeneration
	entry.next = s.options.Now().Add(ciWatchFallbackInterval)
	entry.result = item
	outcome = sharePublishedWithoutEligibleOwner
	for sub := range entry.owners {
		// Pin the first eligible fresh source observation before another
		// completion can replace it, even if this subscriber reads late.
		for i, owned := range sub.entries {
			if owned != entry || claim.version < sub.minimum[i] {
				continue
			}
			if sub.targets[i].HeadSHA == "" && sha.MatchString(item.ObservedHeadSHA) && sha.MatchString(item.ObservedBaseSHA) {
				sub.targets[i].HeadSHA = item.ObservedHeadSHA
			}
			if sub.superseded[i] == nil && item.ObservedHeadSHA == sub.targets[i].HeadSHA && sha.MatchString(item.ObservedBaseSHA) && (sub.bases[i] == "" || item.ObservedBaseSHA == sub.bases[i]) && entry.wanted == entry.done {
				outcome = sharePublished
			}
			if sub.bases[i] == "" && item.ObservedHeadSHA == sub.targets[i].HeadSHA && sha.MatchString(item.ObservedBaseSHA) {
				sub.bases[i] = item.ObservedBaseSHA
			}
			// Source movement is terminal for this subscriber's immutable
			// pins, even if authorization delays delivery across later reads.
			// Retain only bounded source proof, never another owner's CI facts.
			if sub.superseded[i] == nil && item.State != "closed" && sha.MatchString(item.ObservedHeadSHA) && sha.MatchString(item.ObservedBaseSHA) && (item.ObservedHeadSHA != sub.targets[i].HeadSHA || sub.bases[i] != "" && item.ObservedBaseSHA != sub.bases[i]) {
				sub.superseded[i] = &CIWatchItem{State: "superseded", Reason: "ci_watch_source_changed", ObservedHeadSHA: item.ObservedHeadSHA, ObservedBaseSHA: item.ObservedBaseSHA, BaseRef: item.BaseRef, GateRef: item.GateRef, ObservedAt: item.ObservedAt, Checks: []SourceCIContext{}}
			}
		}
		select {
		case sub.changed <- struct{}{}:
		default:
		}
	}
}
