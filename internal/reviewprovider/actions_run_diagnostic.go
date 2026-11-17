package reviewprovider

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ActionsRunObservation is a non-atomic, read-only exact-attempt diagnostic.
// Complete jobs are not a CI verdict, progress/death proof or recovery authority.
type ActionsRunObservation struct {
	Run        ActionsHeadRun
	ObservedAt time.Time
	Jobs       []ActionsJob
}

// ObserveActionsRunAttempt brackets at most eight exact-attempt job pages with
// direct run reads. No discovery list, retry, mutation or automatic scan occurs.
// All errors discard the entire observation. The aggregate limit is fifteen
// seconds and ten GETs, with the existing body and provider admission bounds.
func (g *GitHub) ObserveActionsRunAttempt(ctx context.Context, repository string, repositoryID, runID, expectedAttempt int64) (ActionsRunObservation, error) {
	if ctx == nil || !validRepositorySelector(repository) || repositoryID <= 0 || runID <= 0 || expectedAttempt <= 0 {
		return ActionsRunObservation{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return ActionsRunObservation{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	budget := &actionsBudget{}
	path := "/repos/" + repository + "/actions/runs/" + strconv.FormatInt(runID, 10)
	var first githubActionsRun
	if _, err := g.actionsRead(ctx, path, nil, budget, &first); err != nil {
		return ActionsRunObservation{}, err
	}
	if !validActionsRun(first, repository, first.HeadSHA, first.CheckSuiteID) || first.Repository.ID != repositoryID || first.ID != runID || first.RunAttempt != expectedAttempt {
		return ActionsRunObservation{}, invalidResponse(false, http.StatusOK)
	}
	jobs, err := g.actionsJobsBounded(ctx, path+"/attempts/"+strconv.FormatInt(expectedAttempt, 10)+"/jobs", first.Repository.FullName, first, budget, 8, 800)
	if err != nil {
		return ActionsRunObservation{}, err
	}
	var final githubActionsRun
	if _, err = g.actionsRead(ctx, path, nil, budget, &final); err != nil {
		return ActionsRunObservation{}, err
	}
	created, _ := actionsTime(first.CreatedAt)
	finalCreated, _ := actionsTime(final.CreatedAt)
	if !validActionsRun(final, repository, first.HeadSHA, first.CheckSuiteID) || final.ID != first.ID || final.Repository.ID != first.Repository.ID || !strings.EqualFold(final.Repository.FullName, first.Repository.FullName) || final.WorkflowID != first.WorkflowID || final.RunNumber != first.RunNumber || final.RunAttempt != first.RunAttempt || final.Event != first.Event || !created.Equal(finalCreated) || final.Status != first.Status || !sameActionsConclusion(final.Conclusion, first.Conclusion) {
		return ActionsRunObservation{}, invalidResponse(false, http.StatusOK)
	}
	if ctx.Err() != nil {
		return ActionsRunObservation{}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	updated, _ := actionsTime(final.UpdatedAt)
	conclusion, _ := actionsState(final.Status, final.Conclusion)
	return ActionsRunObservation{Run: ActionsHeadRun{ID: final.ID, CheckSuiteID: final.CheckSuiteID, WorkflowID: final.WorkflowID, RunNumber: final.RunNumber, RunAttempt: final.RunAttempt, RepositoryID: final.Repository.ID, Repository: final.Repository.FullName, HeadSHA: final.HeadSHA, Event: final.Event, Status: final.Status, Conclusion: conclusion, CreatedAt: finalCreated, UpdatedAt: updated}, Jobs: jobs, ObservedAt: time.Now().UTC()}, nil
}
