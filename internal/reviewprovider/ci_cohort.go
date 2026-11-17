package reviewprovider

import (
	"context"
	"slices"
	"sync"
	"time"
)

// MaxCICohortTargets bounds fixed enrollment, not concurrent provider work.
const MaxCICohortTargets = 8

// CIGateCohort shares only the initial authority read among pre-enrolled exact
// targets on one adapter. Every target retains its independent final authority,
// candidate, result and Actions rechecks. It is not a cache or admission proof.
// Call Close after the sequential cohort, including an aborted cohort.
type CIGateCohort struct {
	g          *GitHub
	ctx        context.Context
	cancel     context.CancelFunc
	gate       string
	members    map[CITarget]bool
	mu         sync.Mutex
	initial    *RequiredCheckAuthority
	initialErr error
	started    time.Time
	ordered    []CITarget
	batched    map[CITarget]ciObservationCohortResult
}

// NewCIGateCohort enrolls all members before I/O and starts one bounded lifetime.
// Membership cannot grow; each exact target can be consumed once. Different
// credentials require different adapters and therefore different cohorts.
func (g *GitHub) NewCIGateCohort(ctx context.Context, targets []CITarget, gateRef string) (*CIGateCohort, error) {
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || len(targets) == 0 || len(targets) > MaxCICohortTargets || !validRequiredBranch(gateRef) {
		return nil, invalidSelector()
	}
	members := make(map[CITarget]bool, len(targets))
	for _, t := range targets {
		if !validRepositorySelector(t.Repository) || t.RepositoryID <= 0 || !validPullNumber(t.PullNumber) || !lowerCISHA(t.HeadSHA) || !lowerCISHA(t.BaseSHA) || !validRequiredBranch(t.BaseRef) || t.Repository != targets[0].Repository || t.RepositoryID != targets[0].RepositoryID {
			return nil, invalidSelector()
		}
		if _, exists := members[t]; exists {
			return nil, invalidSelector()
		}
		members[t] = false
	}
	bounded, cancel := context.WithTimeout(ctx, CIObservationTimeout)
	return &CIGateCohort{g: g, ctx: bounded, cancel: cancel, gate: gateRef, members: members, ordered: slices.Clone(targets)}, nil
}

func (c *CIGateCohort) Close() {
	if c != nil {
		c.cancel()
	}
}

// ObserveCIGate refuses late enrollment, repeated consumption and concurrent
// use. The caller must use an ordinary independent collector for changed targets.
func (c *CIGateCohort) ObserveCIGate(ctx context.Context, target CITarget, gateRef string) (CIObservation, error) {
	if c == nil || ctx == nil || !c.mu.TryLock() {
		return CIObservation{}, invalidSelector()
	}
	defer c.mu.Unlock()
	used, exists := c.members[target]
	if !exists || used || gateRef != c.gate {
		return CIObservation{}, invalidSelector()
	}
	c.members[target] = true
	deadline, _ := c.ctx.Deadline()
	readCtx, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if c.ctx.Err() != nil {
		cancel()
	}
	if readCtx.Err() != nil {
		return CIObservation{}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	if c.batched != nil {
		row, exists := c.batched[target]
		if !exists {
			return CIObservation{}, invalidSelector()
		}
		return row.Observation, row.Err
	}
	if c.started.IsZero() {
		c.started = time.Now().UTC()
		authority, err := c.g.RequiredCheckAuthority(readCtx, target.Repository, gateRef)
		c.initial, c.initialErr = &authority, err
	}
	if c.initialErr != nil {
		return CIObservation{Target: target, State: "unproven", Reason: "ci_authority_unproven", StartedAt: c.started, ObservedAt: time.Now().UTC()}, &ciCollectionError{stage: "authority", cause: c.initialErr}
	}
	return c.g.observeCIWithInitialAuthority(readCtx, target, gateRef, c.initial, c.started)
}
