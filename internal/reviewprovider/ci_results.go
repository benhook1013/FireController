package reviewprovider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CIResultInventory retains all selected check runs and commit statuses. It
// makes no latest-attempt, required-check satisfaction, or readiness verdict.
type CIResultInventory struct {
	Repository string
	HeadSHA    string
	ObservedAt time.Time
	Checks     []CICheckRun
	Statuses   []CICommitStatus
}

type CICheckRun struct {
	ID, SuiteID, AppID                int64
	Name, HeadSHA, Status, Conclusion string
	StartedAt, CompletedAt            time.Time
}

type CICommitStatus struct {
	ID                   int64
	Context, State       string
	CreatedAt, UpdatedAt time.Time
}

type githubCICheck struct {
	ID          int64   `json:"id,required"`
	Name        string  `json:"name,required"`
	HeadSHA     string  `json:"head_sha,required"`
	Status      string  `json:"status,required"`
	Conclusion  *string `json:"conclusion,required"`
	StartedAt   *string `json:"started_at,required"`
	CompletedAt *string `json:"completed_at,required"`
	Suite       struct {
		ID int64 `json:"id,required"`
	} `json:"check_suite,required"`
	App struct {
		ID int64 `json:"id,required"`
	} `json:"app,required"`
}

type githubCIStatus struct {
	ID        int64  `json:"id,required"`
	Context   string `json:"context,required"`
	State     string `json:"state,required"`
	CreatedAt string `json:"created_at,required"`
	UpdatedAt string `json:"updated_at,required"`
}

// CIResults reads only the exact immutable commit selectors. The check filter
// is explicitly all on every page; statuses retain historical entries too.
// Count observations bracket collection and reject the reference endpoint's
// 1,000-suite truncation boundary. They do not establish an atomic snapshot or
// claim a full suite inventory.
// https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference
// https://docs.github.com/en/rest/commits/statuses#list-commit-statuses-for-a-reference
func (g *GitHub) CIResults(ctx context.Context, repository, head string) (CIResultInventory, error) {
	if ctx == nil || !validRepositorySelector(repository) || !validSHA(head) || strings.ToLower(head) != head {
		return CIResultInventory{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return CIResultInventory{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	budget := &actionsBudget{}
	for attempt := 0; ; attempt++ {
		out, err := g.ciResultsOnce(ctx, repository, head, budget)
		if err != errCIInventoryChanged || attempt == 1 || ctx.Err() != nil {
			return out, err
		}
		// A newly published check can move the finite inventory between reads.
		// Discard the entire partial collection and try the same immutable
		// selector once, sharing the original time/page/item/byte budgets.
	}
}

var errCIInventoryChanged = providerError("provider_observation_changed", "CI result counts changed during collection.", http.StatusOK, 0, false)

func (g *GitHub) ciResultsOnce(ctx context.Context, repository, head string, budget *actionsBudget) (CIResultInventory, error) {
	path := "/repos/" + repository + "/commits/" + head
	suites, err := g.ciSuiteCount(ctx, path+"/check-suites", head, budget)
	if err != nil {
		return CIResultInventory{}, err
	}
	checks, err := g.ciChecks(ctx, path+"/check-runs", head, budget)
	if err != nil {
		return CIResultInventory{}, err
	}
	statuses, err := g.ciStatuses(ctx, path+"/statuses", budget)
	if err != nil {
		return CIResultInventory{}, err
	}
	finalSuites, err := g.ciSuiteCount(ctx, path+"/check-suites", head, budget)
	if err != nil {
		return CIResultInventory{}, err
	}
	distinct := make(map[int64]bool)
	for _, check := range checks {
		distinct[check.SuiteID] = true
	}
	if finalSuites != suites {
		return CIResultInventory{}, errCIInventoryChanged
	}
	if int64(len(distinct)) > suites {
		return CIResultInventory{}, ciInventoryIncomplete()
	}
	if ctx.Err() != nil {
		return CIResultInventory{}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	return CIResultInventory{Repository: repository, HeadSHA: head, ObservedAt: time.Now().UTC(), Checks: checks, Statuses: statuses}, nil
}

func ciInventoryIncomplete() error {
	return providerError("provider_inventory_incomplete", "The complete CI result inventory could not be established.", http.StatusOK, 0, false)
}

func (g *GitHub) ciSuiteCount(ctx context.Context, path, head string, budget *actionsBudget) (int64, error) {
	budget.pages++
	if budget.pages > githubMaxPages {
		return 0, invalidResponse(false, http.StatusOK)
	}
	var envelope struct {
		Total  int64 `json:"total_count,required"`
		Suites []struct {
			ID      int64  `json:"id,required"`
			HeadSHA string `json:"head_sha,required"`
			App     struct {
				ID int64 `json:"id,required"`
			} `json:"app,required"`
		} `json:"check_suites,required"`
	}
	headers, err := g.actionsRead(ctx, path, url.Values{"page": {"1"}, "per_page": {"1"}}, budget, &envelope)
	if err != nil {
		return 0, err
	}
	budget.items += len(envelope.Suites)
	if envelope.Total < 0 || envelope.Total > githubMaxItems || envelope.Suites == nil || len(envelope.Suites) > 1 || (envelope.Total == 0) != (len(envelope.Suites) == 0) || budget.items > githubMaxItems {
		return 0, invalidResponse(false, http.StatusOK)
	}
	for _, suite := range envelope.Suites {
		if suite.ID <= 0 || suite.HeadSHA != head || suite.App.ID <= 0 {
			return 0, invalidResponse(false, http.StatusOK)
		}
	}
	if !g.validCISuiteCountLinks(headers.Values("Link"), path, envelope.Total) {
		return 0, invalidResponse(false, http.StatusOK)
	}
	if envelope.Total >= 1000 {
		return 0, ciInventoryIncomplete()
	}
	return envelope.Total, nil
}

// The count request intentionally reads only page one with per_page=1. Links
// never drive further requests, but a supplied next link must retain that exact
// selector and cannot contradict the observed count.
func (g *GitHub) validCISuiteCountLinks(headers []string, path string, total int64) bool {
	base, err := url.Parse(g.baseURL)
	if err != nil {
		return false
	}
	found := false
	for _, header := range headers {
		if strings.TrimSpace(header) == "" {
			return false
		}
		links, err := parseLinkHeader(header)
		if err != nil {
			return false
		}
		for _, link := range links {
			if !link.next {
				continue
			}
			target, err := url.Parse(link.target)
			if found || total <= 1 || err != nil || target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host) || target.User != nil || target.Fragment != "" || !validPaginationPath(target, strings.TrimRight(base.Path, "/"), path) {
				return false
			}
			query, err := url.ParseQuery(target.RawQuery)
			if err != nil || len(query) != 2 || len(query["page"]) != 1 || query.Get("page") != "2" || len(query["per_page"]) != 1 || query.Get("per_page") != "1" {
				return false
			}
			found = true
		}
	}
	return true
}

func (g *GitHub) ciChecks(ctx context.Context, path, head string, budget *actionsBudget) ([]CICheckRun, error) {
	rows := make([]CICheckRun, 0)
	seen := map[int64]bool{}
	total := int64(-1)
	fixed := url.Values{"filter": {"all"}}
	for page := int64(1); ; page++ {
		budget.pages++
		if budget.pages > githubMaxPages {
			return nil, invalidResponse(false, http.StatusOK)
		}
		query := url.Values{"filter": {"all"}, "page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		var envelope struct {
			Total  int64           `json:"total_count,required"`
			Checks []githubCICheck `json:"check_runs,required"`
		}
		headers, err := g.actionsRead(ctx, path, query, budget, &envelope)
		if err != nil {
			return nil, err
		}
		budget.items += len(envelope.Checks)
		if envelope.Checks == nil || len(envelope.Checks) > githubPageSize || budget.items > githubMaxItems || envelope.Total < 0 || envelope.Total > githubMaxItems {
			return nil, invalidResponse(false, http.StatusOK)
		}
		changed := total >= 0 && total != envelope.Total
		total = envelope.Total
		for _, raw := range envelope.Checks {
			row, ok := normalizeCICheck(raw, head)
			if !ok || seen[row.ID] {
				return nil, invalidResponse(false, http.StatusOK)
			}
			seen[row.ID] = true
			rows = append(rows, row)
		}
		next, more, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, fixed)
		if err != nil || more && (next != page+1 || len(envelope.Checks) == 0 || int64(len(rows)) >= total) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if changed {
			return nil, errCIInventoryChanged
		}
		if !more {
			if int64(len(rows)) != total {
				return nil, invalidResponse(false, http.StatusOK)
			}
			return rows, nil
		}
	}
}

func (g *GitHub) ciStatuses(ctx context.Context, path string, budget *actionsBudget) ([]CICommitStatus, error) {
	rows := make([]CICommitStatus, 0)
	seen := map[int64]bool{}
	for page := int64(1); ; page++ {
		budget.pages++
		if budget.pages > githubMaxPages {
			return nil, invalidResponse(false, http.StatusOK)
		}
		query := url.Values{"page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		var raw []githubCIStatus
		headers, err := g.actionsRead(ctx, path, query, budget, &raw)
		if err != nil {
			return nil, err
		}
		budget.items += len(raw)
		if raw == nil || len(raw) > githubPageSize || budget.items > githubMaxItems {
			return nil, invalidResponse(false, http.StatusOK)
		}
		for _, value := range raw {
			row, ok := normalizeCIStatus(value)
			if !ok || seen[row.ID] {
				return nil, invalidResponse(false, http.StatusOK)
			}
			seen[row.ID] = true
			rows = append(rows, row)
		}
		next, more, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, nil)
		if err != nil || more && (next != page+1 || len(raw) == 0) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !more {
			return rows, nil
		}
	}
}

func normalizeCICheck(raw githubCICheck, head string) (CICheckRun, bool) {
	conclusion, ok := actionsState(raw.Status, raw.Conclusion)
	if !ok || raw.ID <= 0 || raw.Suite.ID <= 0 || raw.App.ID <= 0 || raw.HeadSHA != head || !validRequiredContext(raw.Name) {
		return CICheckRun{}, false
	}
	row := CICheckRun{ID: raw.ID, SuiteID: raw.Suite.ID, AppID: raw.App.ID, Name: raw.Name, HeadSHA: raw.HeadSHA, Status: raw.Status, Conclusion: conclusion}
	if raw.StartedAt != nil {
		row.StartedAt, ok = actionsTime(*raw.StartedAt)
		if !ok {
			return CICheckRun{}, false
		}
	}
	if raw.CompletedAt != nil {
		row.CompletedAt, ok = actionsTime(*raw.CompletedAt)
		if !ok || !validCIInterval(raw.Status, conclusion, row.StartedAt, row.CompletedAt) {
			return CICheckRun{}, false
		}
	}
	if (raw.Status == "completed") != (raw.CompletedAt != nil) || raw.Status == "in_progress" && raw.StartedAt == nil {
		return CICheckRun{}, false
	}
	return row, true
}

func normalizeCIStatus(raw githubCIStatus) (CICommitStatus, bool) {
	created, createdOK := actionsTime(raw.CreatedAt)
	updated, updatedOK := actionsTime(raw.UpdatedAt)
	if raw.ID <= 0 || !validRequiredContext(raw.Context) || !createdOK || !updatedOK || updated.Before(created) {
		return CICommitStatus{}, false
	}
	switch raw.State {
	case "error", "failure", "pending", "success":
		return CICommitStatus{ID: raw.ID, Context: raw.Context, State: raw.State, CreatedAt: created, UpdatedAt: updated}, true
	}
	return CICommitStatus{}, false
}
