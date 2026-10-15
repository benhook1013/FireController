package reviewprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PullReviewObservation retains provider facts, not a readiness verdict. Two
// matching bounded scans detect movement but do not form an atomic snapshot.
type PullReviewObservation struct {
	Target           CITarget           `json:"target"`
	State            string             `json:"state"`
	Reason           string             `json:"reason"`
	ObservedAt       time.Time          `json:"observed_at"`
	PullState        string             `json:"pull_state"`
	Draft            bool               `json:"draft"`
	Mergeable        string             `json:"mergeable"`
	MergeStateStatus string             `json:"merge_state_status"`
	ReviewDecision   *string            `json:"review_decision"`
	Threads          []PullReviewThread `json:"threads"`
}

type PullReviewThread struct {
	ID       string `json:"id"`
	Resolved bool   `json:"resolved"`
	Outdated bool   `json:"outdated"`
}

// Selected fields follow https://docs.github.com/en/graphql/reference/pulls .
// Repository identity belongs to the selected base repository, not a fork.
const pullReviewStateQuery = `query FireControllerPullReviewState($owner: String!, $name: String!, $number: Int!, $after: String) {
 repository(owner: $owner, name: $name) {
  fullDatabaseId nameWithOwner
  pullRequest(number: $number) {
   number headRefOid baseRefOid baseRefName state isDraft mergeable mergeStateStatus reviewDecision
   reviewThreads(first: 100, after: $after) {
    totalCount nodes { id isResolved isOutdated }
    pageInfo { hasNextPage endCursor }
   }
  }
 }
}`

type githubPullReviewThread struct {
	ID       string `json:"id,required"`
	Resolved bool   `json:"isResolved,required"`
	Outdated bool   `json:"isOutdated,required"`
}
type githubPullReviewState struct {
	Number         int64   `json:"number,required"`
	Head           string  `json:"headRefOid,required"`
	Base           string  `json:"baseRefOid,required"`
	BaseRef        string  `json:"baseRefName,required"`
	State          string  `json:"state,required"`
	Draft          bool    `json:"isDraft,required"`
	Mergeable      string  `json:"mergeable,required"`
	MergeState     string  `json:"mergeStateStatus,required"`
	ReviewDecision *string `json:"reviewDecision,required"`
	Threads        struct {
		Total    int64                    `json:"totalCount,required"`
		Nodes    []githubPullReviewThread `json:"nodes,required"`
		PageInfo struct {
			Next   bool    `json:"hasNextPage,required"`
			Cursor *string `json:"endCursor,required"`
		} `json:"pageInfo,required"`
	} `json:"reviewThreads,required"`
}
type githubPullReviewResponse struct {
	Data struct {
		Repository struct {
			ID   string                `json:"fullDatabaseId,required"`
			Name string                `json:"nameWithOwner,required"`
			Pull githubPullReviewState `json:"pullRequest,required"`
		} `json:"repository,required"`
	} `json:"data,required"`
	Errors []struct{} `json:"errors"`
}

type pullReviewBudget struct{ pages, bytes int }

// ObservePullReviewState performs only fixed GraphQL queries for one explicitly
// selected comparison. It never posts a review or infers readiness from gates.
func (g *GitHub) ObservePullReviewState(ctx context.Context, target CITarget) (out PullReviewObservation, err error) {
	if ctx == nil || !validRepositorySelector(target.Repository) || target.RepositoryID <= 0 || !validPullNumber(target.PullNumber) || !lowerCISHA(target.HeadSHA) || !lowerCISHA(target.BaseSHA) || !validRequiredBranch(target.BaseRef) {
		return out, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return out, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out = PullReviewObservation{Target: target, State: "unproven", Reason: "review_state_unproven"}
	defer func() { out.ObservedAt = time.Now().UTC() }()
	budget := &pullReviewBudget{}
	first, changed, err := g.pullReviewScan(ctx, target, budget, false)
	if err != nil {
		return out, err
	}
	if changed {
		out.Reason = "review_state_changed"
		return out, nil
	}
	second, changed, err := g.pullReviewScan(ctx, target, budget, true)
	if err != nil {
		return out, err
	}
	if changed || !samePullReviewFacts(first, second) || !slices.Equal(first.Threads, second.Threads) {
		out.Reason = "review_state_changed"
		return out, nil
	}
	if ctx.Err() != nil {
		return out, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	second.State, second.Reason = "complete", ""
	return second, nil
}

func (g *GitHub) pullReviewScan(ctx context.Context, target CITarget, budget *pullReviewBudget, subsequent bool) (PullReviewObservation, bool, error) {
	out := PullReviewObservation{Target: target, Threads: []PullReviewThread{}}
	parts := strings.Split(target.Repository, "/")
	var cursor *string
	cursors := make(map[string]bool)
	ids := make(map[string]bool)
	var total int64 = -1
	for {
		if ctx.Err() != nil {
			return out, false, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
		}
		if budget.pages >= githubMaxPages {
			return out, false, invalidResponse(false, http.StatusOK)
		}
		body, _ := json.Marshal(struct {
			Query     string `json:"query"`
			Variables struct {
				Owner  string  `json:"owner"`
				Name   string  `json:"name"`
				Number int64   `json:"number"`
				After  *string `json:"after"`
			} `json:"variables"`
		}{Query: pullReviewStateQuery, Variables: struct {
			Owner  string  `json:"owner"`
			Name   string  `json:"name"`
			Number int64   `json:"number"`
			After  *string `json:"after"`
		}{parts[0], parts[1], target.PullNumber, cursor}})
		requestCtx, cancel := context.WithTimeout(ctx, githubRequestLimit)
		data, headers, status, err := g.pullReviewPage(requestCtx, body)
		cancel()
		budget.pages++
		if err != nil {
			return out, false, err
		}
		if status != http.StatusOK {
			return out, false, statusError(&http.Response{StatusCode: status, Header: headers}, false)
		}
		budget.bytes += len(data)
		var response githubPullReviewResponse
		if budget.bytes > githubMaxAggregate || decodeProviderJSON(data, &response) != nil || len(response.Errors) != 0 {
			return out, false, invalidResponse(false, status)
		}
		repo, pull := response.Data.Repository, response.Data.Repository.Pull
		repoID, idErr := strconv.ParseInt(repo.ID, 10, 64)
		if idErr != nil || repoID <= 0 || strconv.FormatInt(repoID, 10) != repo.ID || !validRepositorySelector(repo.Name) || !validPullReviewMetadata(pull) {
			return out, false, invalidResponse(false, status)
		}
		matched := repoID == target.RepositoryID && strings.EqualFold(repo.Name, target.Repository) && pull.Number == target.PullNumber && pull.Head == target.HeadSHA && pull.Base == target.BaseSHA && pull.BaseRef == target.BaseRef
		if !matched {
			if total >= 0 || subsequent {
				return out, true, nil
			}
			return out, false, invalidResponse(false, status)
		}
		facts := PullReviewObservation{Target: target, PullState: pull.State, Draft: pull.Draft, Mergeable: pull.Mergeable, MergeStateStatus: pull.MergeState, ReviewDecision: pull.ReviewDecision}
		if total >= 0 && !samePullReviewFacts(out, facts) {
			return out, true, nil
		}
		if total < 0 {
			out.PullState, out.Draft, out.Mergeable, out.MergeStateStatus, out.ReviewDecision = facts.PullState, facts.Draft, facts.Mergeable, facts.MergeStateStatus, facts.ReviewDecision
			total = pull.Threads.Total
		}
		threads := pull.Threads
		if threads.Total < 0 || threads.Total > githubMaxItems || total != threads.Total || len(threads.Nodes) > githubPageSize || len(out.Threads)+len(threads.Nodes) > int(total) {
			return out, false, invalidResponse(false, status)
		}
		for _, node := range threads.Nodes {
			if !validIdentifierText(node.ID, 256) || ids[node.ID] {
				return out, false, invalidResponse(false, status)
			}
			ids[node.ID] = true
			out.Threads = append(out.Threads, PullReviewThread{node.ID, node.Resolved, node.Outdated})
		}
		end := threads.PageInfo.Cursor
		if end != nil && (!validIdentifierText(*end, 1024) || cursors[*end]) {
			return out, false, invalidResponse(false, status)
		}
		if end != nil {
			cursors[*end] = true
		}
		if threads.PageInfo.Next {
			if len(threads.Nodes) == 0 || len(out.Threads) >= int(total) || end == nil {
				return out, false, invalidResponse(false, status)
			}
			cursor = end
			continue
		}
		if len(out.Threads) != int(total) || len(threads.Nodes) > 0 && end == nil || len(threads.Nodes) == 0 && end != nil {
			return out, false, invalidResponse(false, status)
		}
		slices.SortFunc(out.Threads, func(a, b PullReviewThread) int { return strings.Compare(a.ID, b.ID) })
		return out, false, nil
	}
}

func validPullReviewMetadata(p githubPullReviewState) bool {
	if !validPullNumber(p.Number) || !lowerCISHA(p.Head) || !lowerCISHA(p.Base) || !validRequiredBranch(p.BaseRef) {
		return false
	}
	switch p.State {
	case "OPEN", "CLOSED", "MERGED":
	default:
		return false
	}
	switch p.Mergeable {
	case "MERGEABLE", "CONFLICTING", "UNKNOWN":
	default:
		return false
	}
	switch p.MergeState {
	case "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE":
	default:
		return false
	}
	if p.ReviewDecision != nil {
		switch *p.ReviewDecision {
		case "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED":
		default:
			return false
		}
	}
	return true
}
func samePullReviewFacts(a, b PullReviewObservation) bool {
	return a.Target == b.Target && a.PullState == b.PullState && a.Draft == b.Draft && a.Mergeable == b.Mergeable && a.MergeStateStatus == b.MergeStateStatus && (a.ReviewDecision == nil && b.ReviewDecision == nil || a.ReviewDecision != nil && b.ReviewDecision != nil && *a.ReviewDecision == *b.ReviewDecision)
}

// GraphQL is a read-only POST but needs a JSON content type. Keep the ordinary
// shared client, authorization, redirect policy and body bound unchanged.
func (g *GitHub) pullReviewPage(ctx context.Context, body []byte) ([]byte, http.Header, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.baseURL, "/")+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request could not be prepared.", 0, 0, false)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "FireController")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	response, err := g.httpClient.Do(request)
	if err != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request failed.", 0, 0, false)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.Header.Clone(), response.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil || len(data) > githubMaxBody {
		return nil, nil, 0, invalidResponse(false, response.StatusCode)
	}
	return data, response.Header.Clone(), response.StatusCode, nil
}
