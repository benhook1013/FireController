package reviewprovider

import (
	"context"
	"slices"
	"time"
)

type ciObservationCohortResult struct {
	Observation CIObservation
	Err         error
}

// observeCIGateSourceCohort is an unconnected integration seam. It does not
// replace the public sequential collector or change scheduler enrollment. The
// two-member ceiling preserves the current lifecycle cohort size; callers must
// retain its account/project reservation and publication checks.
func (g *GitHub) observeCIGateSourceCohort(ctx context.Context, targets []CITarget, gateRef string) ([]ciObservationCohortResult, error) {
	return g.observeCIGateSourceCohortMode(ctx, targets, gateRef, false)
}

func (g *GitHub) observeCIGateBatchedCohort(ctx context.Context, targets []CITarget, gateRef string) ([]ciObservationCohortResult, error) {
	return g.observeCIGateSourceCohortMode(ctx, targets, gateRef, true)
}

func (g *GitHub) observeCIGateSourceCohortMode(ctx context.Context, targets []CITarget, gateRef string, batchCounts bool) ([]ciObservationCohortResult, error) {
	if len(targets) > 2 {
		return nil, invalidSelector()
	}
	targets = slices.Clone(targets)
	admission, err := g.NewCIGateCohort(ctx, targets, gateRef)
	if err != nil {
		return nil, err
	}
	defer admission.Close()
	ctx = admission.ctx
	started := time.Now().UTC()
	out := make([]ciObservationCohortResult, len(targets))
	stages := make([]string, len(targets))
	caches := make([]map[string]githubCIMerge, len(targets))
	active := make([]bool, len(targets))
	finish := func(i int, err error) {
		noteCIBatchFailure(ctx, err)
		out[i].Err = err
		if err != nil {
			out[i].Err = &ciCollectionError{stage: stages[i], cause: err}
		}
		out[i].Observation.ObservedAt = time.Now().UTC()
		active[i] = false
	}
	for i, t := range targets {
		out[i].Observation = CIObservation{Target: t, State: "unproven", Reason: "ci_authority_unproven", StartedAt: started}
		stages[i] = "authority"
		caches[i] = make(map[string]githubCIMerge, 2)
	}
	authority, authorityErr := g.RequiredCheckAuthority(ctx, targets[0].Repository, gateRef)
	for i := range out {
		out[i].Observation.Authority = authority
		out[i].Observation.Authority.Checks = slices.Clone(authority.Checks)
	}
	if authorityErr != nil || authority.State != "complete" || authority.Checks == nil {
		for i := range out {
			finish(i, authorityErr)
		}
		return out, nil
	}
	if len(authority.Checks) > 128 {
		for i := range out {
			out[i].Observation.Reason = "ci_required_context_capacity"
			finish(i, nil)
		}
		return out, nil
	}
	candidates, batchErr := g.pullCICandidateCohort(ctx, targets, caches)
	if batchErr != nil {
		for i := range out {
			stages[i] = "candidate"
			out[i].Observation.Reason = "ci_source_unavailable"
			finish(i, batchErr)
		}
		return out, nil
	}
	eligible := make([]bool, len(targets))
	for i, row := range candidates {
		o := &out[i].Observation
		t := targets[i]
		candidate := row.Candidate
		o.Candidate = &candidate
		o.MergeState = candidate.MergeState
		stages[i] = "candidate"
		if row.Err != nil {
			o.Reason = "ci_source_unavailable"
			finish(i, row.Err)
			continue
		}
		if !matchesCITarget(candidate, t) && !NonstrictHeadCICandidate(candidate, t, o.Authority) {
			o.Reason = "ci_target_unproven"
			if candidate.State == "unproven" {
				switch candidate.Reason {
				case "ci_merge_identity_unproven", "ci_merge_base_mismatch", "ci_merge_pending", "ci_candidate_changed", "ci_target_changed":
					o.Reason = candidate.Reason
				}
			}
			finish(i, nil)
			continue
		}
		eligible[i] = true
	}
	readers := make([]ciInventoryReader, len(targets))
	for i := range readers {
		readers[i] = g.CIResults
	}
	if batchCounts {
		readers = g.prepareCIBodyInventories(ctx, out, eligible)
	}
	for i := range out {
		if !eligible[i] {
			continue
		}
		o := &out[i].Observation
		if err := g.collectCIBodyWithResults(ctx, *o.Candidate, o, &stages[i], readers[i]); err != nil {
			finish(i, err)
			continue
		}
		if o.State != "satisfied" && o.State != "not_required" {
			finish(i, nil)
			continue
		}
		// No provisional assessment is publishable while later members collect.
		o.State, o.Reason = "unproven", "ci_observation_changed"
		active[i] = true
	}
	var finals []CITarget
	var finalCaches []map[string]githubCIMerge
	var indexes []int
	for i := range out {
		if !active[i] {
			continue
		}
		stages[i] = "authority_recheck"
		a, err := g.RequiredCheckAuthority(ctx, targets[i].Repository, gateRef)
		if err != nil || !sameCIAuthority(out[i].Observation.Authority, a) {
			finish(i, err)
			continue
		}
		finals = append(finals, targets[i])
		finalCaches = append(finalCaches, caches[i])
		indexes = append(indexes, i)
	}
	if len(finals) > 0 {
		candidates, err := g.pullCICandidateCohort(ctx, finals, finalCaches)
		if err != nil {
			for _, i := range indexes {
				stages[i] = "candidate_recheck"
				finish(i, err)
			}
		} else {
			for j, row := range candidates {
				i := indexes[j]
				o := &out[i].Observation
				t := targets[i]
				stages[i] = "candidate_recheck"
				if row.Err != nil || !sameCICandidate(*o.Candidate, row.Candidate) || !matchesCITarget(row.Candidate, t) && !NonstrictHeadCICandidate(row.Candidate, t, o.Authority) {
					finish(i, row.Err)
					continue
				}
			}
			readers := make([]ciInventoryReader, len(out))
			for i := range readers {
				readers[i] = g.CIResults
			}
			if batchCounts {
				readers = g.prepareCIFinalInventories(ctx, out, active)
			}
			for _, i := range indexes {
				if !active[i] {
					continue
				}
				o := &out[i].Observation
				finish(i, g.finishCIResultsWithReader(ctx, *o.Candidate, o, &stages[i], readers[i]))
			}
		}
	}
	return out, nil
}
