package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type ciResultsPairResult struct {
	Inventory CIResultInventory
	Err       error
}

// ciResultsPair is an unconnected two-member experiment. It changes only count
// witness transport: all check runs and historical statuses still use the
// original REST collectors. One shared 15s deadline and private member budgets
// cover both attempts. This is a conservative total cap versus two separate
// sequential 15s calls under an outer 40s collector budget. A healthy, fully
// closed member is never recollected, even if its partner later times out;
// the outer collector must still enforce its own publication deadline.
// The GraphQL combined-body cap can conservatively refuse responses that two
// separate REST reads would admit; no identical failure-surface claim is made.
func (g *GitHub) ciResultsPair(ctx context.Context, repository string, heads [2]string) (out [2]ciResultsPairResult) {
	fail := func(e error) [2]ciResultsPairResult { return [2]ciResultsPairResult{{Err: e}, {Err: e}} }
	if ctx == nil || !validRepositorySelector(repository) {
		return fail(invalidSelector())
	}
	for _, h := range heads {
		if !validSHA(h) || strings.ToLower(h) != h {
			return fail(invalidSelector())
		}
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return fail(providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	budgets := [2]*actionsBudget{{}, {}}
	active := [2]bool{true, true}
	for attempt := 0; attempt < 2; attempt++ {
		before := g.ciResultsPairCounts(ctx, repository, heads, budgets, active)
		var checks [2][]CICheckRun
		var statuses [2][]CICommitStatus
		survivors := [2]bool{}
		for i := range heads {
			if !active[i] {
				continue
			}
			out[i] = ciResultsPairResult{Err: before[i].Err}
			noteCIBatchFailure(ctx, out[i].Err)
			if out[i].Err != nil {
				continue
			}
			path := "/repos/" + repository + "/commits/" + heads[i]
			checks[i], out[i].Err = g.ciChecks(ctx, path+"/check-runs", heads[i], budgets[i])
			noteCIBatchFailure(ctx, out[i].Err)
			if out[i].Err != nil {
				continue
			}
			statuses[i], out[i].Err = g.ciStatuses(ctx, path+"/statuses", budgets[i])
			noteCIBatchFailure(ctx, out[i].Err)
			survivors[i] = out[i].Err == nil
		}
		after := g.ciResultsPairCounts(ctx, repository, heads, budgets, survivors)
		retry := [2]bool{}
		for i := range heads {
			if !active[i] {
				continue
			}
			if survivors[i] {
				out[i].Err = after[i].Err
				noteCIBatchFailure(ctx, out[i].Err)
				if out[i].Err == nil {
					distinct := map[int64]bool{}
					for _, c := range checks[i] {
						distinct[c.SuiteID] = true
					}
					switch {
					case before[i].Count != after[i].Count:
						out[i].Err = errCIInventoryChanged
					case int64(len(distinct)) > before[i].Count:
						out[i].Err = ciInventoryIncomplete()
					case ctx.Err() != nil:
						out[i].Err = providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
					default:
						out[i].Inventory = CIResultInventory{Repository: repository, HeadSHA: heads[i], ObservedAt: time.Now().UTC(), Checks: checks[i], Statuses: statuses[i]}
					}
				}
			}
			if errors.Is(out[i].Err, errCIInventoryChanged) {
				if attempt == 0 && ctx.Err() == nil {
					retry[i] = true
				} else {
					out[i].Err = providerError("provider_observation_changed", "CI result counts changed during collection.", http.StatusOK, 0, false)
				}
			}
		}
		for i := range out {
			if !errors.Is(out[i].Err, errCIInventoryChanged) {
				noteCIBatchFailure(ctx, out[i].Err)
			}
		}
		if !retry[0] && !retry[1] {
			break
		}
		active = retry
	}
	return out
}

func (g *GitHub) ciResultsPairCounts(ctx context.Context, repository string, heads [2]string, budgets [2]*actionsBudget, active [2]bool) (out [2]ciSuiteCountResult) {
	if active[0] && active[1] {
		return g.ciSuiteCountsBatch(ctx, repository, heads, budgets)
	}
	for i := range heads {
		if !active[i] {
			continue
		}
		if ctx.Err() != nil {
			out[i].Err = providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
			continue
		}
		out[i].Count, out[i].Err = g.ciSuiteCount(ctx, "/repos/"+repository+"/commits/"+heads[i]+"/check-suites", heads[i], budgets[i])
	}
	return out
}
