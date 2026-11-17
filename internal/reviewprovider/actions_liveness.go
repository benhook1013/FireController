package reviewprovider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ActionsLiveness is an explicit diagnostic scan, never a required-CI verdict,
// cancellation authorization, atomic repository snapshot or runner-capacity proof.
// No scheduler calls this method. Each invocation costs at most ten GETs.
type ActionsLiveness struct {
	Repository   string
	RepositoryID int64
	ObservedAt   time.Time
	Runs         []ActionsLivenessRun
}

type ActionsLivenessRun struct {
	ActionsHeadRun
	Aged     bool   // UpdatedAt exceeds the caller's diagnostic threshold, not proof of death.
	Capacity string // Always unknown: workflow status does not establish runner/lock ownership.
}

// ObserveActionsLiveness scans all supported nonterminal states without a head
// filter so superseded-head orphans remain visible. Transitions between status
// pages are not atomic; duplicates or any incomplete page refuse the entire scan.
// Even a successful scan cannot prove absence of runs moving between buckets.
func (g *GitHub) ObserveActionsLiveness(ctx context.Context, repository string, repositoryID int64, age time.Duration) (ActionsLiveness, error) {
	if ctx == nil || !validRepositorySelector(repository) || repositoryID <= 0 || age < time.Hour || age > 30*24*time.Hour {
		return ActionsLiveness{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return ActionsLiveness{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now().UTC()
	out := ActionsLiveness{Repository: repository, RepositoryID: repositoryID, Runs: []ActionsLivenessRun{}}
	budget := &actionsBudget{}
	seen := map[int64]bool{}
	numbers := map[[2]int64]bool{}
	path := "/repos/" + repository + "/actions/runs"
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested"} {
		total, count := -1, 0
		for page := int64(1); ; page++ {
			if page > 2 {
				return ActionsLiveness{}, ciInventoryIncomplete()
			}
			q := url.Values{"status": {status}, "per_page": {"100"}, "page": {strconv.FormatInt(page, 10)}}
			var envelope struct {
				Total int                `json:"total_count,required"`
				Runs  []githubActionsRun `json:"workflow_runs,required"`
			}
			headers, err := g.actionsRead(ctx, path, q, budget, &envelope)
			if err != nil {
				return ActionsLiveness{}, err
			}
			if envelope.Runs == nil || envelope.Total < 0 || len(envelope.Runs) > 100 || (total >= 0 && total != envelope.Total) {
				return ActionsLiveness{}, invalidResponse(false, http.StatusOK)
			}
			if envelope.Total > 200 {
				return ActionsLiveness{}, ciInventoryIncomplete()
			}
			total = envelope.Total
			for _, r := range envelope.Runs {
				number := [2]int64{r.WorkflowID, r.RunNumber}
				if r.Status != status || r.Repository.ID != repositoryID || seen[r.ID] || numbers[number] || !validSHA(r.HeadSHA) || strings.ToLower(r.HeadSHA) != r.HeadSHA || !validActionsRun(r, repository, r.HeadSHA, r.CheckSuiteID) {
					return ActionsLiveness{}, invalidResponse(false, http.StatusOK)
				}
				created, _ := actionsTime(r.CreatedAt)
				updated, _ := actionsTime(r.UpdatedAt)
				if updated.After(start) {
					return ActionsLiveness{}, ciInventoryIncomplete()
				}
				seen[r.ID], numbers[number] = true, true
				out.Runs = append(out.Runs, ActionsLivenessRun{ActionsHeadRun: ActionsHeadRun{ID: r.ID, CheckSuiteID: r.CheckSuiteID, WorkflowID: r.WorkflowID, RunNumber: r.RunNumber, RunAttempt: r.RunAttempt, RepositoryID: r.Repository.ID, Repository: r.Repository.FullName, HeadSHA: r.HeadSHA, Event: r.Event, Status: r.Status, CreatedAt: created, UpdatedAt: updated}, Aged: start.Sub(updated) >= age, Capacity: "unknown"})
			}
			count += len(envelope.Runs)
			next, more, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, url.Values{"status": {status}})
			if err != nil {
				return ActionsLiveness{}, err
			}
			if !more {
				if count != total {
					return ActionsLiveness{}, ciInventoryIncomplete()
				}
				break
			}
			if next != page+1 || len(envelope.Runs) == 0 || count >= total {
				return ActionsLiveness{}, invalidResponse(false, http.StatusOK)
			}
		}
	}
	if ctx.Err() != nil {
		return ActionsLiveness{}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	out.ObservedAt = time.Now().UTC()
	return out, nil
}
