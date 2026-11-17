package reviewprovider

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Candidate cohorts are not wired into a scheduler or public collector. The
// caller must hold the existing account/project reservation and enroll all exact
// targets before reads. Only same-phase branch witnesses are shared.
type ciCandidateCohortResult struct {
	Candidate CICandidate
	Err       error
}

func (g *GitHub) pullCICandidateCohort(ctx context.Context, targets []CITarget, commits []map[string]githubCIMerge) ([]ciCandidateCohortResult, error) {
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || len(targets) < 1 || len(targets) > MaxCICohortTargets || commits != nil && len(commits) != len(targets) {
		return nil, invalidSelector()
	}
	targets = append([]CITarget(nil), targets...)
	seen := map[int64]bool{}
	for _, t := range targets {
		if !validRepositorySelector(t.Repository) || t.RepositoryID <= 0 || !validPullNumber(t.PullNumber) || !lowerCISHA(t.HeadSHA) || !lowerCISHA(t.BaseSHA) || !validRequiredBranch(t.BaseRef) || t.Repository != targets[0].Repository || t.RepositoryID != targets[0].RepositoryID || seen[t.PullNumber] {
			return nil, invalidSelector()
		}
		seen[t.PullNumber] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	states := make([]*ciCandidatePhases, len(targets))
	budgets := make([]actionsBudget, len(targets))
	if commits == nil {
		commits = make([]map[string]githubCIMerge, len(targets))
	}
	for i, t := range targets {
		states[i] = newCICandidatePhases(t.Repository, t.RepositoryID, t.PullNumber, t.HeadSHA)
	}
	stampDone := func(s *ciCandidatePhases) {
		noteCIBatchFailure(ctx, s.err)
		if s.phase == ciCandidateDone && s.result.ObservedAt.IsZero() {
			s.result.ObservedAt = time.Now().UTC()
		}
	}
	cancelPending := func(s *ciCandidatePhases) bool {
		if s.phase == ciCandidateDone {
			return false
		}
		if ctx.Err() != nil {
			s.err = providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
			s.phase = ciCandidateDone
			stampDone(s)
			return true
		}
		return false
	}
	readPull := func(i int, closing bool) {
		s := states[i]
		if cancelPending(s) {
			return
		}
		var p githubCIPull
		_, err := g.actionsRead(ctx, "/repos/"+targets[i].Repository+"/pulls/"+strconv.FormatInt(targets[i].PullNumber, 10), nil, &budgets[i], &p)
		if closing {
			s.acceptFinal(ctx, p, err)
		} else {
			s.acceptFirst(p, err)
		}
		stampDone(s)
	}
	for i := range states {
		readPull(i, false)
	}
	branchPhase := func(phase ciCandidatePhase) {
		groups := map[string][]int{}
		var order []string
		for i, s := range states {
			if s.phase != phase || cancelPending(s) {
				continue
			}
			ref := s.first.Pull.Base.Ref
			if _, ok := groups[ref]; !ok {
				order = append(order, ref)
			}
			groups[ref] = append(groups[ref], i)
		}
		for _, ref := range order {
			members := groups[ref]
			if ctx.Err() != nil {
				for _, i := range members {
					cancelPending(states[i])
				}
				continue
			}
			var budget actionsBudget
			branch, matches, err := g.readCICandidateBranch(ctx, targets[0].Repository, ref, &budget)
			for _, i := range members {
				s := states[i]
				budgets[i].bytes += budget.bytes
				memberErr := err
				if memberErr == nil && budgets[i].bytes > githubMaxAggregate {
					memberErr = invalidResponse(false, http.StatusOK)
				}
				if phase == ciCandidateBefore {
					s.acceptBefore(branch, matches, memberErr)
				} else {
					s.acceptAfter(branch, matches, memberErr)
				}
				stampDone(s)
			}
		}
	}
	branchPhase(ciCandidateBefore)
	for i, s := range states {
		if s.phase != ciCandidateObject || cancelPending(s) {
			continue
		}
		sha := *s.first.MergeCommitSHA
		object, reused := commits[i][sha]
		var err error
		if !reused {
			_, err = g.actionsRead(ctx, "/repos/"+targets[i].Repository+"/git/commits/"+sha, nil, &budgets[i], &object)
		}
		s.acceptObject(object, err, commits[i])
		stampDone(s)
	}
	branchPhase(ciCandidateAfter)
	for i, s := range states {
		if s.phase == ciCandidateFinal {
			readPull(i, true)
		}
	}
	out := make([]ciCandidateCohortResult, len(states))
	for i, s := range states {
		out[i] = ciCandidateCohortResult{Candidate: s.result, Err: s.err}
	}
	return out, nil
}
