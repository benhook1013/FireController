package reviewprovider

import (
	"context"
	"encoding/json"
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
	MergeBaseParentSHA string               `json:"merge_base_parent_sha"`
	BaseRef            string               `json:"base_ref"`
	MergeSHA           string               `json:"merge_sha"`
	MergeState         string               `json:"merge_state"`
	ObservedAt         time.Time            `json:"observed_at"`
	MergeBaseMismatch  *CIMergeBaseMismatch `json:"merge_base_mismatch,omitempty"`
}

// CIMergeBaseMismatch records the distinct as-of identities already read for
// a refused candidate. None is a selected CI commit or permission to merge.
type CIMergeBaseMismatch struct {
	PRBaseSHA          string `json:"pr_base_sha"`
	LiveBaseSHA        string `json:"live_base_sha"`
	MergeSHA           string `json:"merge_sha"`
	MergeBaseParentSHA string `json:"merge_base_parent_sha"`
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
func (g *GitHub) PullCICandidate(ctx context.Context, repository string, repositoryID, number int64, expectedHead string) (CICandidate, error) {
	return g.pullCICandidate(ctx, repository, repositoryID, number, expectedHead, nil)
}

// commits is confined to one CI observation and one repository. Only exact
// immutable Git objects may be reused; PR and live-base brackets stay fresh.
func (g *GitHub) pullCICandidate(ctx context.Context, repository string, repositoryID, number int64, expectedHead string, commits map[string]githubCIMerge) (result CICandidate, err error) {
	if ctx == nil || !validRepositorySelector(repository) || repositoryID <= 0 || !validPullNumber(number) || !lowerCISHA(expectedHead) {
		return CICandidate{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return CICandidate{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	defer func() { result.ObservedAt = time.Now().UTC() }()
	p := newCICandidatePhases(repository, repositoryID, number, expectedHead)
	budget := &actionsBudget{}
	path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10)
	var first githubCIPull
	_, readErr := g.actionsRead(ctx, path, nil, budget, &first)
	p.acceptFirst(first, readErr)
	if p.phase == ciCandidateBefore {
		branch, matches, readErr := g.readCICandidateBranch(ctx, repository, p.first.Pull.Base.Ref, budget)
		p.acceptBefore(branch, matches, readErr)
	}
	if p.phase == ciCandidateObject {
		merge, reused := commits[*p.first.MergeCommitSHA]
		var readErr error
		if !reused {
			_, readErr = g.actionsRead(ctx, "/repos/"+repository+"/git/commits/"+*p.first.MergeCommitSHA, nil, budget, &merge)
		}
		p.acceptObject(merge, readErr, commits)
	}
	if p.phase == ciCandidateAfter {
		branch, matches, readErr := g.readCICandidateBranch(ctx, repository, p.first.Pull.Base.Ref, budget)
		p.acceptAfter(branch, matches, readErr)
	}
	if p.phase == ciCandidateFinal {
		var final githubCIPull
		_, readErr := g.actionsRead(ctx, path, nil, budget, &final)
		p.acceptFinal(ctx, final, readErr)
	}
	return p.result, p.err
}
