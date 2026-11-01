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

// CICandidate binds observed PR facts and, where available, its test merge.
// It does not select the commit whose CI applies or establish CI success.
type CICandidate struct {
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repository_id"`
	PullNumber   int64  `json:"pull_number"`
	HeadSHA      string `json:"head_sha"`
	// BaseSHA is the observed PR base.sha, not an independently read branch tip.
	BaseSHA string `json:"base_sha"`
	// MergeBaseParentSHA is populated only after the live base branch and merge parents bind.
	MergeBaseParentSHA string    `json:"merge_base_parent_sha"`
	BaseRef            string    `json:"base_ref"`
	MergeSHA           string    `json:"merge_sha"`
	MergeState         string    `json:"merge_state"`
	ObservedAt         time.Time `json:"observed_at"`
}

type githubCIPull struct {
	Pull           githubPull `json:"-"`
	Mergeable      *bool      `json:"mergeable,required"`
	MergeCommitSHA *string    `json:"merge_commit_sha,required"`
}

func (p *githubCIPull) UnmarshalJSON(data []byte) error {
	type wire githubCIPull
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	// Reuse the ordinary PR wire contract without relaxing either its fields
	// or the separately required nullable merge fields.
	if err := decodeProviderJSON(data, &value.Pull); err != nil {
		return err
	}
	*p = githubCIPull(value)
	return nil
}

type githubCIMerge struct {
	SHA     string `json:"sha,required"`
	Parents []struct {
		SHA string `json:"sha,required"`
	} `json:"parents,required"`
}

type githubCIBranch struct {
	Name   string `json:"name,required"`
	Commit struct {
		SHA string `json:"sha,required"`
	} `json:"commit,required"`
}

func lowerCISHA(sha string) bool { return validSHA(sha) && sha == strings.ToLower(sha) }

func validCIPull(raw githubCIPull, number int64) bool {
	if _, err := normalizePull(raw.Pull, number); err != nil {
		return false
	}
	return lowerCISHA(raw.Pull.Head.SHA) && lowerCISHA(raw.Pull.Base.SHA) && (raw.MergeCommitSHA == nil || lowerCISHA(*raw.MergeCommitSHA))
}

func sameCIPull(a, b githubCIPull) bool {
	return a.Pull.Number == b.Pull.Number && a.Pull.State == b.Pull.State && a.Pull.Merged == b.Pull.Merged && a.Pull.Head.SHA == b.Pull.Head.SHA && a.Pull.Base.SHA == b.Pull.Base.SHA && a.Pull.Base.Ref == b.Pull.Base.Ref && a.Pull.Base.Repo.ID == b.Pull.Base.Repo.ID && strings.EqualFold(a.Pull.Base.Repo.FullName, b.Pull.Base.Repo.FullName) && sameCIBool(a.Mergeable, b.Mergeable) && sameActionsConclusion(a.MergeCommitSHA, b.MergeCommitSHA)
}

func sameCIBool(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// PullCICandidate reads the selected PR before and after inspecting its test
// merge and independently selected base branch. A merge's parent order is not
// assumed; both parents must bind to the head and a stable live branch tip.
// https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request
// https://docs.github.com/en/rest/git/commits#get-a-commit-object
func (g *GitHub) PullCICandidate(ctx context.Context, repository string, repositoryID, number int64, expectedHead string) (result CICandidate, err error) {
	if ctx == nil || !validRepositorySelector(repository) || repositoryID <= 0 || !validPullNumber(number) || !lowerCISHA(expectedHead) {
		return CICandidate{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return CICandidate{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	defer func() { result.ObservedAt = time.Now().UTC() }()
	result = CICandidate{State: "unproven", Reason: "ci_target_changed", Repository: repository, RepositoryID: repositoryID, PullNumber: number, HeadSHA: expectedHead}
	budget := &actionsBudget{}
	path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10)
	var first githubCIPull
	if _, err = g.actionsRead(ctx, path, nil, budget, &first); err != nil {
		return result, err
	}
	if !validCIPull(first, number) {
		return result, invalidResponse(false, http.StatusOK)
	}
	if first.Pull.Base.Repo.ID != repositoryID || !strings.EqualFold(first.Pull.Base.Repo.FullName, repository) || first.Pull.State != "open" || first.Pull.Merged || first.Pull.Head.SHA != expectedHead {
		return result, nil
	}
	result.Repository = first.Pull.Base.Repo.FullName
	result.BaseSHA, result.BaseRef = first.Pull.Base.SHA, first.Pull.Base.Ref
	result.Reason, result.MergeState = "ci_merge_pending", "pending"
	mergeVerified := false
	mergeBaseParent := ""
	if first.Mergeable != nil {
		if !*first.Mergeable {
			result.MergeState, result.Reason = "conflicting", ""
		} else {
			if first.MergeCommitSHA == nil || *first.MergeCommitSHA == first.Pull.Head.SHA || *first.MergeCommitSHA == first.Pull.Base.SHA {
				return result, invalidResponse(false, http.StatusOK)
			}
			result.MergeState, result.Reason = "unproven", "ci_merge_identity_unproven"
			branchPath := "/repos/" + repository + "/branches/" + url.PathEscape(first.Pull.Base.Ref)
			readBranch := func() (githubCIBranch, bool, error) {
				var branch githubCIBranch
				if _, readErr := g.actionsRead(ctx, branchPath, nil, budget, &branch); readErr != nil {
					var missing *Error
					if errors.As(readErr, &missing) && missing.Code == "provider_not_found" && missing.Status == http.StatusNotFound {
						return branch, false, nil
					}
					return branch, false, readErr
				}
				if !lowerCISHA(branch.Commit.SHA) {
					return branch, false, invalidResponse(false, http.StatusOK)
				}
				return branch, branch.Name == first.Pull.Base.Ref, nil
			}
			before, branchMatches, branchErr := readBranch()
			if branchErr != nil {
				return result, branchErr
			}
			if !branchMatches {
				return result, nil
			}
			var merge githubCIMerge
			if _, err = g.actionsRead(ctx, "/repos/"+repository+"/git/commits/"+*first.MergeCommitSHA, nil, budget, &merge); err != nil {
				return result, err
			}
			if !lowerCISHA(merge.SHA) {
				return result, invalidResponse(false, http.StatusOK)
			}
			for _, parent := range merge.Parents {
				if !lowerCISHA(parent.SHA) {
					return result, invalidResponse(false, http.StatusOK)
				}
			}
			after, branchMatches, branchErr := readBranch()
			if branchErr != nil {
				return result, branchErr
			}
			mergeBaseParent = before.Commit.SHA
			mergeVerified = branchMatches && after.Commit.SHA == before.Commit.SHA && merge.SHA == *first.MergeCommitSHA && len(merge.Parents) == 2 && merge.Parents[0].SHA != merge.Parents[1].SHA && ((merge.Parents[0].SHA == mergeBaseParent && merge.Parents[1].SHA == first.Pull.Head.SHA) || (merge.Parents[1].SHA == mergeBaseParent && merge.Parents[0].SHA == first.Pull.Head.SHA))
			result.MergeState, result.Reason = "unproven", "ci_merge_identity_unproven"
			if !mergeVerified && branchMatches && after.Commit.SHA == before.Commit.SHA && merge.SHA == *first.MergeCommitSHA && len(merge.Parents) == 2 && merge.Parents[0].SHA != merge.Parents[1].SHA && (merge.Parents[0].SHA == first.Pull.Head.SHA || merge.Parents[1].SHA == first.Pull.Head.SHA) {
				// The head and merge object are attributable, but their other
				// parent differs from the independently observed live base tip.
				// Matching the cached PR base.sha is not sufficient proof.
				result.Reason = "ci_merge_base_mismatch"
			}
		}
	}
	var final githubCIPull
	if _, err = g.actionsRead(ctx, path, nil, budget, &final); err != nil {
		return result, err
	}
	if !validCIPull(final, number) {
		return result, invalidResponse(false, http.StatusOK)
	}
	if !sameCIPull(first, final) {
		result.Reason = "ci_candidate_changed"
		return result, nil
	}
	if ctx.Err() != nil {
		return result, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	if mergeVerified {
		result.State, result.Reason, result.MergeState, result.MergeSHA, result.MergeBaseParentSHA = "complete", "", "verified", *first.MergeCommitSHA, mergeBaseParent
	} else if result.MergeState == "conflicting" {
		result.State = "complete"
	}
	return result, nil
}
