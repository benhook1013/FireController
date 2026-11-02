package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ActionsJob struct {
	ID          int64     `json:"id"`
	CheckRunID  int64     `json:"check_run_id"`
	Name        string    `json:"name"`
	HeadSHA     string    `json:"head_sha"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// ActionsAttempt proves a current attempt inventory, not successful required CI.
// Expected app identity and applicable required contexts are caller-owned facts.
type ActionsAttempt struct {
	State        string       `json:"state"`
	Reason       string       `json:"reason"`
	Repository   string       `json:"repository"`
	RepositoryID int64        `json:"repository_id"`
	HeadSHA      string       `json:"head_sha"`
	CheckSuiteID int64        `json:"check_suite_id"`
	RunID        int64        `json:"run_id"`
	WorkflowID   int64        `json:"workflow_id"`
	RunAttempt   int64        `json:"run_attempt"`
	RunNumber    int64        `json:"run_number"`
	Event        string       `json:"event"`
	Status       string       `json:"status"`
	Conclusion   string       `json:"conclusion"`
	ObservedAt   time.Time    `json:"observed_at"`
	Jobs         []ActionsJob `json:"jobs"`
}

type githubActionsRun struct {
	ID           int64   `json:"id,required"`
	HeadSHA      string  `json:"head_sha,required"`
	CheckSuiteID int64   `json:"check_suite_id,required"`
	WorkflowID   int64   `json:"workflow_id,required"`
	RunAttempt   int64   `json:"run_attempt,required"`
	RunNumber    int64   `json:"run_number,required"`
	Status       string  `json:"status,required"`
	Conclusion   *string `json:"conclusion,required"`
	Event        string  `json:"event,required"`
	CreatedAt    string  `json:"created_at,required"`
	UpdatedAt    string  `json:"updated_at,required"`
	Repository   struct {
		ID       int64  `json:"id,required"`
		FullName string `json:"full_name,required"`
	} `json:"repository,required"`
}

type githubActionsJob struct {
	ID          int64   `json:"id,required"`
	RunID       int64   `json:"run_id,required"`
	RunAttempt  *int64  `json:"run_attempt"`
	HeadSHA     string  `json:"head_sha,required"`
	CheckRunURL string  `json:"check_run_url,required"`
	Name        string  `json:"name,required"`
	Status      string  `json:"status,required"`
	Conclusion  *string `json:"conclusion,required"`
	StartedAt   *string `json:"started_at,required"`
	CompletedAt *string `json:"completed_at,required"`
}

func (j *githubActionsJob) UnmarshalJSON(data []byte) error {
	type wire githubActionsJob
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["run_attempt"]; present && value.RunAttempt == nil {
		return errors.New("invalid attempt identity")
	}
	*j = githubActionsJob(value)
	return nil
}

type actionsBudget struct{ bytes, items, pages int }

func (g *GitHub) actionsRead(ctx context.Context, path string, query url.Values, budget *actionsBudget, dst any) (http.Header, error) {
	data, headers, status, err := g.requestBytes(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(&http.Response{StatusCode: status, Header: headers}, false)
	}
	budget.bytes += len(data)
	if budget.bytes > githubMaxAggregate || decodeProviderJSON(data, dst) != nil {
		return nil, invalidResponse(false, http.StatusOK)
	}
	return headers, nil
}

func actionsState(status string, conclusion *string) (string, bool) {
	if status == "completed" {
		if conclusion == nil {
			return "", false
		}
		switch *conclusion {
		case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
			return *conclusion, true
		}
		return "", false
	}
	switch status {
	case "queued", "in_progress", "waiting", "pending", "requested":
		return "", conclusion == nil
	}
	return "", false
}

func actionsTime(value string) (time.Time, bool) {
	instant, err := time.Parse(time.RFC3339Nano, value)
	return instant.UTC(), err == nil && !instant.IsZero()
}

func validActionsRun(r githubActionsRun, repository, head string, suite int64) bool {
	_, stateOK := actionsState(r.Status, r.Conclusion)
	created, createdOK := actionsTime(r.CreatedAt)
	updated, updatedOK := actionsTime(r.UpdatedAt)
	return r.ID > 0 && suite > 0 && r.CheckSuiteID == suite && r.WorkflowID > 0 && r.RunAttempt > 0 && r.RunNumber > 0 && r.HeadSHA == head && validSHA(r.HeadSHA) && r.HeadSHA == strings.ToLower(r.HeadSHA) && r.Repository.ID > 0 && validRepositorySelector(r.Repository.FullName) && strings.EqualFold(r.Repository.FullName, repository) && validIdentifierText(r.Event, 64) && stateOK && createdOK && updatedOK && !updated.Before(created)
}

// CurrentActionsAttempt reads only the selected suite/head and its current
// attempt jobs. The final current-run read rejects a rerun during collection.
// GitHub documents the two run selectors and the attempt-specific jobs path:
// https://docs.github.com/en/rest/actions/workflow-runs#list-workflow-runs-for-a-repository
// https://docs.github.com/en/rest/actions/workflow-jobs#list-jobs-for-a-workflow-run-attempt
func (g *GitHub) CurrentActionsAttempt(ctx context.Context, repository, head string, suiteID int64) (result ActionsAttempt, err error) {
	if ctx == nil || !validRepositorySelector(repository) || !validSHA(head) || strings.ToLower(head) != head || suiteID <= 0 {
		return ActionsAttempt{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return ActionsAttempt{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result = ActionsAttempt{State: "unproven", Reason: "ci_attempt_not_proven", Repository: repository, HeadSHA: head, CheckSuiteID: suiteID, Jobs: []ActionsJob{}}
	defer func() { result.ObservedAt = time.Now().UTC() }()
	budget := &actionsBudget{}
	path := "/repos/" + repository + "/actions/runs"
	rows, err := g.actionsRuns(ctx, path, repository, head, suiteID, budget)
	if err != nil {
		return result, err
	}
	if len(rows) != 1 {
		result.Reason = "ci_attempt_missing"
		if len(rows) > 1 {
			result.Reason = "ci_attempt_ambiguous"
		}
		return result, nil
	}
	selected := rows[0]
	currentPath := path + "/" + strconv.FormatInt(selected.ID, 10)
	var current githubActionsRun
	if _, err = g.actionsRead(ctx, currentPath, nil, budget, &current); err != nil {
		return result, err
	}
	if !validActionsRun(current, repository, head, suiteID) || current.ID != selected.ID || current.WorkflowID != selected.WorkflowID || current.RunNumber != selected.RunNumber || current.Repository.ID != selected.Repository.ID {
		return result, invalidResponse(false, http.StatusOK)
	}
	jobsPath := currentPath + "/attempts/" + strconv.FormatInt(current.RunAttempt, 10) + "/jobs"
	jobs, err := g.actionsJobs(ctx, jobsPath, current.Repository.FullName, current, budget)
	if err != nil {
		return result, err
	}
	var final githubActionsRun
	if _, err = g.actionsRead(ctx, currentPath, nil, budget, &final); err != nil {
		return result, err
	}
	if !validActionsRun(final, final.Repository.FullName, final.HeadSHA, final.CheckSuiteID) {
		return result, invalidResponse(false, http.StatusOK)
	}
	created, _ := actionsTime(current.CreatedAt)
	finalCreated, _ := actionsTime(final.CreatedAt)
	if final.ID != current.ID || final.WorkflowID != current.WorkflowID || final.RunAttempt != current.RunAttempt || final.RunNumber != current.RunNumber || final.Repository.ID != current.Repository.ID || !strings.EqualFold(final.Repository.FullName, current.Repository.FullName) || final.HeadSHA != current.HeadSHA || final.CheckSuiteID != current.CheckSuiteID || final.Event != current.Event || !finalCreated.Equal(created) {
		result.Reason = "ci_attempt_changed"
		return result, nil
	}
	if final.Status != current.Status || !sameActionsConclusion(final.Conclusion, current.Conclusion) {
		result.Reason = "ci_observation_changed"
		return result, nil
	}
	if ctx.Err() != nil {
		return result, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	result.State, result.Reason = "complete", ""
	result.RunID, result.WorkflowID, result.RunAttempt = current.ID, current.WorkflowID, current.RunAttempt
	result.Repository, result.RepositoryID = current.Repository.FullName, current.Repository.ID
	result.Event, result.RunNumber = current.Event, current.RunNumber
	result.Status, result.Conclusion = current.Status, ""
	if current.Conclusion != nil {
		result.Conclusion = *current.Conclusion
	}
	result.Jobs = jobs
	return result, nil
}

func sameActionsConclusion(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (g *GitHub) actionsRuns(ctx context.Context, path, repository, head string, suiteID int64, budget *actionsBudget) ([]githubActionsRun, error) {
	rows := make([]githubActionsRun, 0)
	seen := map[int64]bool{}
	total := int64(-1)
	fixed := url.Values{"check_suite_id": {strconv.FormatInt(suiteID, 10)}, "head_sha": {head}}
	for page := int64(1); page <= githubMaxPages; page++ {
		budget.pages++
		if budget.pages > githubMaxPages {
			return nil, invalidResponse(false, http.StatusOK)
		}
		query := url.Values{"check_suite_id": {strconv.FormatInt(suiteID, 10)}, "head_sha": {head}}
		query.Set("page", strconv.FormatInt(page, 10))
		query.Set("per_page", strconv.Itoa(githubPageSize))
		var envelope struct {
			TotalCount int64              `json:"total_count,required"`
			Runs       []githubActionsRun `json:"workflow_runs,required"`
		}
		headers, err := g.actionsRead(ctx, path, query, budget, &envelope)
		if err != nil {
			return nil, err
		}
		budget.items += len(envelope.Runs)
		if envelope.Runs == nil || len(envelope.Runs) > githubPageSize || budget.items > githubMaxItems || envelope.TotalCount < 0 || envelope.TotalCount > githubMaxItems || total >= 0 && total != envelope.TotalCount {
			return nil, invalidResponse(false, http.StatusOK)
		}
		total = envelope.TotalCount
		for _, row := range envelope.Runs {
			if seen[row.ID] || !validActionsRun(row, repository, head, suiteID) {
				return nil, invalidResponse(false, http.StatusOK)
			}
			seen[row.ID] = true
			rows = append(rows, row)
		}
		next, exists, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, fixed)
		if err != nil || exists && (next != page+1 || int64(len(rows)) >= total || len(envelope.Runs) == 0) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !exists {
			if int64(len(rows)) != total {
				return nil, invalidResponse(false, http.StatusOK)
			}
			return rows, nil
		}
	}
	return nil, invalidResponse(false, http.StatusOK)
}

func (g *GitHub) actionsJobs(ctx context.Context, path, repository string, run githubActionsRun, budget *actionsBudget) ([]ActionsJob, error) {
	rows := make([]ActionsJob, 0)
	seen, checks := map[int64]bool{}, map[int64]bool{}
	total := int64(-1)
	for page := int64(1); page <= githubMaxPages; page++ {
		budget.pages++
		if budget.pages > githubMaxPages {
			return nil, invalidResponse(false, http.StatusOK)
		}
		query := url.Values{"page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		var envelope struct {
			TotalCount int64              `json:"total_count,required"`
			Jobs       []githubActionsJob `json:"jobs,required"`
		}
		headers, err := g.actionsRead(ctx, path, query, budget, &envelope)
		if err != nil {
			return nil, err
		}
		budget.items += len(envelope.Jobs)
		if envelope.Jobs == nil || len(envelope.Jobs) > githubPageSize || budget.items > githubMaxItems || envelope.TotalCount < 0 || envelope.TotalCount > githubMaxItems || total >= 0 && total != envelope.TotalCount {
			return nil, invalidResponse(false, http.StatusOK)
		}
		total = envelope.TotalCount
		for _, raw := range envelope.Jobs {
			job, valid := normalizeActionsJob(raw, repository, run)
			if !valid || seen[job.ID] || checks[job.CheckRunID] {
				return nil, invalidResponse(false, http.StatusOK)
			}
			seen[job.ID], checks[job.CheckRunID] = true, true
			rows = append(rows, job)
		}
		next, exists, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, nil)
		if err != nil || exists && (next != page+1 || int64(len(rows)) >= total || len(envelope.Jobs) == 0) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !exists {
			if int64(len(rows)) != total {
				return nil, invalidResponse(false, http.StatusOK)
			}
			return rows, nil
		}
	}
	return nil, invalidResponse(false, http.StatusOK)
}

func normalizeActionsJob(raw githubActionsJob, repository string, run githubActionsRun) (ActionsJob, bool) {
	job := ActionsJob{ID: raw.ID, Name: raw.Name, HeadSHA: raw.HeadSHA, Status: raw.Status}
	conclusion, valid := actionsState(raw.Status, raw.Conclusion)
	if !valid || raw.ID <= 0 || raw.RunID != run.ID || raw.RunAttempt != nil && *raw.RunAttempt != run.RunAttempt || raw.HeadSHA != run.HeadSHA || !validPlainText(raw.Name, 255) || strings.TrimSpace(raw.Name) == "" {
		return ActionsJob{}, false
	}
	job.Conclusion = conclusion
	prefix := githubAPIURL + "/repos/" + repository + "/check-runs/"
	if !strings.HasPrefix(raw.CheckRunURL, prefix) {
		return ActionsJob{}, false
	}
	identifier := strings.TrimPrefix(raw.CheckRunURL, prefix)
	check, err := strconv.ParseInt(identifier, 10, 64)
	if err != nil || check <= 0 || strconv.FormatInt(check, 10) != identifier {
		return ActionsJob{}, false
	}
	job.CheckRunID = check
	if raw.StartedAt != nil {
		var ok bool
		job.StartedAt, ok = actionsTime(*raw.StartedAt)
		if !ok {
			return ActionsJob{}, false
		}
	}
	if raw.CompletedAt != nil {
		var ok bool
		job.CompletedAt, ok = actionsTime(*raw.CompletedAt)
		if !ok || !validCIInterval(raw.Status, conclusion, job.StartedAt, job.CompletedAt) {
			return ActionsJob{}, false
		}
	}
	if (raw.Status == "completed") != (raw.CompletedAt != nil) || raw.Status == "in_progress" && raw.StartedAt == nil {
		return ActionsJob{}, false
	}
	return job, true
}
