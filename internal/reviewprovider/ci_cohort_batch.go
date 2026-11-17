package reviewprovider

import "context"

// PrepareBatched is explicitly opt-in. It gathers only the exact original two
// members before sequential consumption; it is not late-subscriber freshness or
// publication authority. Callers retain their per-member binding/publication
// checks, account reservation, common deadline and cooldown rules.
func (c *CIGateCohort) PrepareBatched(ctx context.Context) error {
	if c == nil || ctx == nil || !c.mu.TryLock() {
		return invalidSelector()
	}
	defer c.mu.Unlock()
	if len(c.ordered) != 2 || c.batched != nil || c.initial != nil {
		return invalidSelector()
	}
	for _, used := range c.members {
		if used {
			return invalidSelector()
		}
	}
	deadline, _ := c.ctx.Deadline()
	bounded, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if c.ctx.Err() != nil {
		cancel()
	}
	if bounded.Err() != nil {
		return providerTransportError(bounded, bounded.Err(), false)
	}
	// A multi-member operation must not inherit the first PR's attribution.
	// Request totals/caller accounting remain complete; shared target attribution
	// is deliberately absent rather than charged twice or assigned to one member.
	bounded = context.WithValue(bounded, githubTargetContextKey{}, githubTargetKey{})
	// Preparation cancellation is separate from the consumption lifetime.
	preparation, stopPreparation := context.WithCancel(bounded)
	defer stopPreparation()
	stopState := &ciBatchStop{cancel: stopPreparation}
	preparation = context.WithValue(preparation, ciBatchStopKey{}, stopState)
	rows, err := c.g.observeCIGateBatchedCohort(preparation, c.ordered, c.gate)
	if cause := stopState.failure(); cause != nil {
		return &CIBatchAccountFailure{Cause: cause}
	}
	if err != nil {
		return err
	}
	if len(rows) != len(c.ordered) {
		return invalidSelector()
	}
	retained := make(map[CITarget]ciObservationCohortResult, len(rows))
	for i, t := range c.ordered {
		if rows[i].Observation.Target != t {
			return invalidSelector()
		}
		retained[t] = rows[i]
	}
	c.batched = retained
	return nil
}
