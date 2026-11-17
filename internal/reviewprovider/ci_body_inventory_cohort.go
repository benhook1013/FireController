package reviewprovider

import "context"

type ciInventoryReader func(context.Context, string, string) (CIResultInventory, error)

// prepareCIBodyInventories batches only independent same-phase inventories.
// An empty test merge still requires a separate, later head phase. Results are
// private to this invocation and never replace final observation witnesses.
func (g *GitHub) prepareCIBodyInventories(ctx context.Context, rows []ciObservationCohortResult, eligible []bool) []ciInventoryReader {
	readers := make([]ciInventoryReader, len(rows))
	retained := make([]map[string]ciResultsPairResult, len(rows))
	var indexes []int
	var heads []string
	for i := range rows {
		retained[i] = make(map[string]ciResultsPairResult, 2)
		i := i
		readers[i] = func(_ context.Context, repository, head string) (CIResultInventory, error) {
			if repository != rows[i].Observation.Target.Repository {
				return CIResultInventory{}, invalidSelector()
			}
			r, ok := retained[i][head]
			if !ok {
				return CIResultInventory{}, invalidSelector()
			}
			return r.Inventory, r.Err
		}
		if !eligible[i] || len(rows[i].Observation.Authority.Checks) == 0 {
			continue
		}
		o := &rows[i].Observation
		head := o.Target.HeadSHA
		if o.Candidate.MergeState == "verified" {
			head = o.Candidate.MergeSHA
		}
		indexes = append(indexes, i)
		heads = append(heads, head)
	}
	collect := func(indexes []int, heads []string) {
		if len(indexes) == 2 {
			repository := rows[indexes[0]].Observation.Target.Repository
			pair := g.ciResultsPair(ctx, repository, [2]string{heads[0], heads[1]})
			for j, i := range indexes {
				retained[i][heads[j]] = pair[j]
			}
		} else if len(indexes) == 1 {
			i := indexes[0]
			inventory, err := g.CIResults(ctx, rows[i].Observation.Target.Repository, heads[0])
			retained[i][heads[0]] = ciResultsPairResult{Inventory: inventory, Err: err}
		}
	}
	collect(indexes, heads)
	var fallbackIndexes []int
	var fallbackHeads []string
	for j, i := range indexes {
		o := &rows[i].Observation
		r := retained[i][heads[j]]
		if o.Candidate.MergeState == "verified" && r.Err == nil && len(r.Inventory.Checks) == 0 && len(r.Inventory.Statuses) == 0 {
			fallbackIndexes = append(fallbackIndexes, i)
			fallbackHeads = append(fallbackHeads, o.Target.HeadSHA)
		}
	}
	collect(fallbackIndexes, fallbackHeads)
	return readers
}

// Final selected inventories are collected only after all participating members
// pass their independent final authority and candidate witnesses. Empty-merge
// rechecks remain later than each member's Actions recheck, in the old order.
func (g *GitHub) prepareCIFinalInventories(ctx context.Context, rows []ciObservationCohortResult, active []bool) []ciInventoryReader {
	readers := make([]ciInventoryReader, len(rows))
	var indexes []int
	for i := range rows {
		readers[i] = g.CIResults
		if active[i] && rows[i].Observation.Results != nil {
			indexes = append(indexes, i)
		}
	}
	if len(indexes) != 2 {
		return readers
	}
	first, second := rows[indexes[0]].Observation, rows[indexes[1]].Observation
	pair := g.ciResultsPair(ctx, first.Target.Repository, [2]string{first.SelectedCommitSHA, second.SelectedCommitSHA})
	for j, i := range indexes {
		o := rows[i].Observation
		result := pair[j]
		used := false
		readers[i] = func(ctx context.Context, repository, head string) (CIResultInventory, error) {
			if used {
				return g.CIResults(ctx, repository, head)
			}
			used = true
			if repository != o.Target.Repository || head != o.SelectedCommitSHA {
				return CIResultInventory{}, invalidSelector()
			}
			return result.Inventory, result.Err
		}
	}
	return readers
}
