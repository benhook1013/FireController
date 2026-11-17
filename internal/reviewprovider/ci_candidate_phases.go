package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// These phases hold one candidate's original classification rules. They neither
// acquire provider facts nor establish CI authority. A coordinator must retain
// the original per-candidate deadlines, target admission and phase order.
type ciCandidatePhase uint8

const (
	ciCandidateFirst ciCandidatePhase = iota
	ciCandidateBefore
	ciCandidateObject
	ciCandidateAfter
	ciCandidateFinal
	ciCandidateDone
)

type ciCandidatePhases struct {
	result          CICandidate
	err             error
	phase           ciCandidatePhase
	first           githubCIPull
	before          githubCIBranch
	merge           githubCIMerge
	mergeVerified   bool
	mergeBaseParent string
	mismatch        *CIMergeBaseMismatch
}

// The caller validates selectors and starts the bounded context before reading.
// ObservedAt remains owned by the caller after its final read or refusal.
func newCICandidatePhases(repository string, repositoryID, number int64, expectedHead string) *ciCandidatePhases {
	return &ciCandidatePhases{result: CICandidate{State: "unproven", Reason: "ci_target_changed", Repository: repository, RepositoryID: repositoryID, PullNumber: number, HeadSHA: expectedHead}, phase: ciCandidateFirst}
}
func (p *ciCandidatePhases) stop(err error) { p.err = err; p.phase = ciCandidateDone }
func (p *ciCandidatePhases) enter(want ciCandidatePhase, err error) bool {
	if p.phase != want {
		p.stop(invalidSelector())
		return false
	}
	if err != nil {
		p.stop(err)
		return false
	}
	return true
}

func (p *ciCandidatePhases) acceptFirst(first githubCIPull, err error) {
	if !p.enter(ciCandidateFirst, err) {
		return
	}
	if !validCIPull(first, p.result.PullNumber) {
		p.stop(invalidResponse(false, http.StatusOK))
		return
	}
	if first.Pull.Base.Repo.ID != p.result.RepositoryID || !strings.EqualFold(first.Pull.Base.Repo.FullName, p.result.Repository) || first.Pull.State != "open" || first.Pull.Merged || first.Pull.Head.SHA != p.result.HeadSHA {
		p.stop(nil)
		return
	}
	p.first = first
	p.result.Repository = first.Pull.Base.Repo.FullName
	p.result.BaseSHA, p.result.BaseRef = first.Pull.Base.SHA, first.Pull.Base.Ref
	p.result.Reason, p.result.MergeState = "ci_merge_pending", "pending"
	p.phase = ciCandidateFinal
	if first.Mergeable == nil {
		return
	}
	if !*first.Mergeable {
		p.result.MergeState, p.result.Reason = "conflicting", ""
		return
	}
	if first.MergeCommitSHA == nil || *first.MergeCommitSHA == first.Pull.Head.SHA || *first.MergeCommitSHA == first.Pull.Base.SHA {
		p.stop(invalidResponse(false, http.StatusOK))
		return
	}
	p.result.MergeState, p.result.Reason = "unproven", "ci_merge_identity_unproven"
	p.phase = ciCandidateBefore
}

func (p *ciCandidatePhases) acceptBefore(branch githubCIBranch, matches bool, err error) {
	if !p.enter(ciCandidateBefore, err) {
		return
	}
	if !matches {
		p.stop(nil)
		return
	}
	p.before = branch
	p.phase = ciCandidateObject
}

func (p *ciCandidatePhases) acceptObject(merge githubCIMerge, err error, commits map[string]githubCIMerge) {
	if !p.enter(ciCandidateObject, err) {
		return
	}
	if !lowerCISHA(merge.SHA) {
		p.stop(invalidResponse(false, http.StatusOK))
		return
	}
	for _, parent := range merge.Parents {
		if !lowerCISHA(parent.SHA) {
			p.stop(invalidResponse(false, http.StatusOK))
			return
		}
	}
	if commits != nil && merge.SHA == *p.first.MergeCommitSHA && len(merge.Parents) == 2 && merge.Parents[0].SHA != merge.Parents[1].SHA {
		commits[merge.SHA] = merge
	}
	p.merge = merge
	p.phase = ciCandidateAfter
}

func (p *ciCandidatePhases) acceptAfter(after githubCIBranch, matches bool, err error) {
	if !p.enter(ciCandidateAfter, err) {
		return
	}
	first, merge, before := p.first, p.merge, p.before
	p.mergeBaseParent = before.Commit.SHA
	p.mergeVerified = matches && after.Commit.SHA == before.Commit.SHA && merge.SHA == *first.MergeCommitSHA && len(merge.Parents) == 2 && merge.Parents[0].SHA != merge.Parents[1].SHA && ((merge.Parents[0].SHA == p.mergeBaseParent && merge.Parents[1].SHA == first.Pull.Head.SHA) || (merge.Parents[1].SHA == p.mergeBaseParent && merge.Parents[0].SHA == first.Pull.Head.SHA))
	p.result.MergeState, p.result.Reason = "unproven", "ci_merge_identity_unproven"
	if !p.mergeVerified && matches && after.Commit.SHA == before.Commit.SHA && merge.SHA == *first.MergeCommitSHA && len(merge.Parents) == 2 && merge.Parents[0].SHA != merge.Parents[1].SHA && (merge.Parents[0].SHA == first.Pull.Head.SHA || merge.Parents[1].SHA == first.Pull.Head.SHA) {
		// The head and merge object are attributable, but their other parent differs
		// from the independently stable live base. Cached PR base alone is not proof.
		p.result.Reason = "ci_merge_base_mismatch"
		parent := merge.Parents[0].SHA
		if parent == first.Pull.Head.SHA {
			parent = merge.Parents[1].SHA
		}
		p.mismatch = &CIMergeBaseMismatch{PRBaseSHA: first.Pull.Base.SHA, LiveBaseSHA: before.Commit.SHA, MergeSHA: merge.SHA, MergeBaseParentSHA: parent}
	}
	p.phase = ciCandidateFinal
}

func (p *ciCandidatePhases) acceptFinal(ctx context.Context, final githubCIPull, err error) {
	if !p.enter(ciCandidateFinal, err) {
		return
	}
	if !validCIPull(final, p.result.PullNumber) {
		p.stop(invalidResponse(false, http.StatusOK))
		return
	}
	if !sameCIPull(p.first, final) {
		p.result.Reason = "ci_candidate_changed"
		p.stop(nil)
		return
	}
	if ctx.Err() != nil {
		p.stop(providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false))
		return
	}
	if p.mergeVerified {
		p.result.State, p.result.Reason, p.result.MergeState, p.result.MergeSHA, p.result.MergeBaseParentSHA = "complete", "", "verified", *p.first.MergeCommitSHA, p.mergeBaseParent
	} else if p.result.MergeState == "conflicting" {
		p.result.State = "complete"
	} else if p.result.Reason == "ci_merge_base_mismatch" {
		p.result.MergeBaseMismatch = p.mismatch
	}
	p.stop(nil)
}

// readCICandidateBranch is shared only within an explicitly ordered phase.
// A missing branch remains a refused witness, not a transport failure.
func (g *GitHub) readCICandidateBranch(ctx context.Context, repository, baseRef string, budget *actionsBudget) (githubCIBranch, bool, error) {
	var branch githubCIBranch
	path := "/repos/" + repository + "/branches/" + url.PathEscape(baseRef)
	if _, err := g.actionsRead(ctx, path, nil, budget, &branch); err != nil {
		var missing *Error
		if errors.As(err, &missing) && missing.Code == "provider_not_found" && missing.Status == http.StatusNotFound {
			return branch, false, nil
		}
		return branch, false, err
	}
	if !lowerCISHA(branch.Commit.SHA) {
		return branch, false, invalidResponse(false, http.StatusOK)
	}
	return branch, branch.Name == baseRef, nil
}
