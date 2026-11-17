package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// A lifecycle collection can satisfy already waiting plain gate subscribers.
// Selection happens before provider reads. Later subscribers keep their next
// generation minimum; optional workflow demand never joins this narrower read.
// Cancellation of a subscriber cannot cancel the independent lifecycle work.
func (e *Engine) beginLifecycleCIShare(ctx context.Context, target lifecycleTarget, s *lifecycleSchedule) *ciWatchClaim {
	// Prepared batch evidence predates this admission point. Never credit it
	// to a watch selected now; that watch retains its own fresh collection.
	if target.ciPreparedBatch {
		return nil
	}
	if target.lane != lifecycleEnrichment || target.comparison.state != "open" {
		return nil
	}
	outcome := shareNotDue
	defer func() { s.sharing.record(s.options.Now(), outcome) }()
	d := target.due
	if !target.forceCI && d.ciHead == target.comparison.head && d.ciBaseSHA == target.comparison.baseSHA && d.ciBaseRef == target.comparison.baseRef && s.options.Now().Before(d.ci) {
		return nil
	}
	outcome = shareNoEligibleOwner
	b := ciWatchBinding{project: target.project, capability: target.configuration.CapabilityID, fingerprint: target.fingerprint, account: target.account, repository: target.repository}
	key := ciWatchKey{binding: b, number: target.number}
	s.mu.Lock()
	entry := s.watches[key]
	var owners []*ciWatchSubscription
	if entry != nil && !entry.inflight && len(ciWatchWorkflowUnion(entry, nil)) == 0 {
		for owner := range entry.owners {
			owners = append(owners, owner)
		}
	}
	s.mu.Unlock()
	authorized := false
	for _, owner := range owners {
		current, err := e.ciWatchBinding(ctx, owner.scope)
		if err == nil && current == b {
			authorized = true
			break
		}
	}
	if !authorized {
		return nil
	}
	outcome = shareDemandChanged
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[key] != entry || entry.inflight || len(entry.owners) == 0 || len(ciWatchWorkflowUnion(entry, nil)) != 0 || !ciWatchHasDemand(entry, s.options.Now()) {
		return nil
	}
	outcome = shareClaimed
	entry.started++
	entry.inflight = true
	entry.lastAttempt = s.options.Now()
	entry.inflightSHAHints = nil
	entry.inflightSHAOverflow = false
	sharedCtx, cancel := context.WithCancel(ctx)
	entry.cancel = cancel
	return &ciWatchClaim{lifecycleShared: true, entry: entry, generation: entry.wanted, metadataGeneration: entry.metadataWanted, version: entry.started, ctx: sharedCtx}
}

// Only an already validated exact observation is projected. No optional
// workflow facts or approximate cache hit can be promoted into required CI.
func lifecycleCIWatchItem(project string, o reviewprovider.CIObservation) (CIWatchItem, bool) {
	summary := summarizeSourceCI(o)
	checks, err := sourceCIContextDetails(&o)
	if err != nil {
		return CIWatchItem{}, false
	}
	item := CIWatchItem{State: "pending", Reason: summary.Reason, ObservedHeadSHA: o.Target.HeadSHA, ObservedBaseSHA: o.Target.BaseSHA, BaseRef: o.Target.BaseRef, GateRef: o.Authority.Branch, ObservedAt: summary.ObservedAt, CICommitSHA: summary.SelectedCommitSHA, Selection: summary.Selection, Required: summary.Required, Satisfied: summary.Satisfied, Failed: summary.Failed, Pending: summary.Pending, Unproven: summary.Unproven, Checks: checks}
	if o.Reason == "ci_merge_base_mismatch" && o.Candidate != nil && o.Candidate.MergeBaseMismatch != nil {
		facts := *o.Candidate.MergeBaseMismatch
		item.MergeBaseMismatch = &facts
	}
	switch o.State {
	case "satisfied":
		item.State = "passed"
	case "failed":
		item.State = "failed"
	case "not_required":
		item.State = "unconfigured"
	}
	if ciWatchRequiredFailure(o) {
		item.State = "failed"
		item.Reason = "ci_required_checks_failed"
	}
	item.ObservationKey = ciWatchObservationKey(project, o, item)
	return item, true
}
