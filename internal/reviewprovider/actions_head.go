package reviewprovider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ActionsHeadRun struct {
	ID, CheckSuiteID, WorkflowID, RunNumber, RunAttempt, RepositoryID int64
	Repository, HeadSHA, Event, Status, Conclusion                    string
	CreatedAt, UpdatedAt                                              time.Time
}

// ActionsHeadInventory is a complete bounded selected-head inventory, not a
// latest-run, event-applicability, required-check, or passing-CI verdict.
type ActionsHeadInventory struct {
	Repository   string
	HeadSHA      string
	RepositoryID int64
	ObservedAt   time.Time
	Runs         []ActionsHeadRun
}

// ActionsRunsForHead retains all workflows, suites, events and current rerun
// attempt facts returned for the exact commit. A suite filter would hide a
// newer run in another suite, so only head_sha is fixed on every page.
// GitHub caps filtered results at 1,000; that boundary remains unproven here.
// https://docs.github.com/en/rest/actions/workflow-runs#list-workflow-runs-for-a-repository
// https://docs.github.com/en/actions/reference/workflows-and-actions/variables#default-environment-variables
func (g *GitHub) ActionsRunsForHead(ctx context.Context, repository string, repositoryID int64, head string) (ActionsHeadInventory, error) {
	if ctx == nil || !validRepositorySelector(repository) || repositoryID <= 0 || !validSHA(head) || strings.ToLower(head) != head {
		return ActionsHeadInventory{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return ActionsHeadInventory{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := g.actionsHeadRuns(ctx, "/repos/"+repository+"/actions/runs", repository, repositoryID, head, &actionsBudget{})
	if err != nil {
		return ActionsHeadInventory{}, err
	}
	if ctx.Err() != nil {
		return ActionsHeadInventory{}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	return ActionsHeadInventory{Repository: repository, RepositoryID: repositoryID, HeadSHA: head, ObservedAt: time.Now().UTC(), Runs: rows}, nil
}

func (g *GitHub) actionsHeadRuns(ctx context.Context, path, repository string, repositoryID int64, head string, budget *actionsBudget) ([]ActionsHeadRun, error) {
	rows := make([]ActionsHeadRun, 0)
	seen := make(map[int64]bool)
	numbers := make(map[[2]int64]bool)
	total := int64(-1)
	fixed := url.Values{"head_sha": {head}}
	for page := int64(1); ; page++ {
		budget.pages++
		if budget.pages > githubMaxPages {
			return nil, invalidResponse(false, http.StatusOK)
		}
		query := url.Values{"head_sha": {head}, "page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		var envelope struct {
			Total int64              `json:"total_count,required"`
			Runs  []githubActionsRun `json:"workflow_runs,required"`
		}
		headers, err := g.actionsRead(ctx, path, query, budget, &envelope)
		if err != nil {
			return nil, err
		}
		budget.items += len(envelope.Runs)
		if envelope.Runs == nil || len(envelope.Runs) > githubPageSize || budget.items > githubMaxItems || envelope.Total < 0 || envelope.Total > githubMaxItems || total >= 0 && total != envelope.Total {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if envelope.Total >= 1000 {
			return nil, ciInventoryIncomplete()
		}
		total = envelope.Total
		for _, raw := range envelope.Runs {
			number := [2]int64{raw.WorkflowID, raw.RunNumber}
			if !validActionsRun(raw, repository, head, raw.CheckSuiteID) || raw.Repository.ID != repositoryID || seen[raw.ID] || numbers[number] {
				return nil, invalidResponse(false, http.StatusOK)
			}
			seen[raw.ID], numbers[number] = true, true
			created, _ := actionsTime(raw.CreatedAt)
			updated, _ := actionsTime(raw.UpdatedAt)
			conclusion, _ := actionsState(raw.Status, raw.Conclusion)
			rows = append(rows, ActionsHeadRun{ID: raw.ID, CheckSuiteID: raw.CheckSuiteID, WorkflowID: raw.WorkflowID, RunNumber: raw.RunNumber, RunAttempt: raw.RunAttempt, RepositoryID: raw.Repository.ID, Repository: raw.Repository.FullName, HeadSHA: raw.HeadSHA, Event: raw.Event, Status: raw.Status, Conclusion: conclusion, CreatedAt: created, UpdatedAt: updated})
		}
		next, more, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, fixed)
		if err != nil || more && (next != page+1 || len(envelope.Runs) == 0 || int64(len(rows)) >= total) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !more {
			if int64(len(rows)) != total {
				return nil, invalidResponse(false, http.StatusOK)
			}
			return rows, nil
		}
	}
}
