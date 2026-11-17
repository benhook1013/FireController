package reviewprovider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"time"
)

// CIObservationTimeout bounds a complete gate read, including its final rechecks.
const CIObservationTimeout = 40 * time.Second

// CITarget is an explicit observed source assertion, not a discovery selector.
type CITarget struct {
	RepositoryID int64  `json:"repository_id"`
	PullNumber   int64  `json:"pull_number"`
	Repository   string `json:"repository"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	BaseRef      string `json:"base_ref"`
}

// CIObservation retains collected facts, including an empty test-merge
// inventory when it justifies head fallback. Nil pointers mean not fetched.
// Even a satisfied observation is not merge readiness or an atomic provider
// snapshot; its final reads bound detectable movement during collection.
type CIObservation struct {
	Assessment        RequiredCIAssessment   `json:"assessment"`
	Target            CITarget               `json:"target"`
	SelectedCommitSHA string                 `json:"selected_commit_sha"`
	Selection         string                 `json:"selection"`
	MergeState        string                 `json:"merge_state"`
	State             string                 `json:"state"`
	Reason            string                 `json:"reason"`
	StartedAt         time.Time              `json:"started_at"`
	ObservedAt        time.Time              `json:"observed_at"`
	Authority         RequiredCheckAuthority `json:"authority"`
	Candidate         *CICandidate           `json:"candidate,omitempty"`
	Results           *CIResultInventory     `json:"results,omitempty"`
	Actions           *ActionsHeadInventory  `json:"actions,omitempty"`
	Attempts          []ActionsAttempt       `json:"attempts"`
	TestMergeResults  *CIResultInventory     `json:"test_merge_results,omitempty"`
	// Optional attention proof is ephemeral and never joins required CI credit.
	WorkflowAttempts    []ActionsAttempt      `json:"-"`
	WorkflowActions     *ActionsHeadInventory `json:"-"`
	WorkflowHeadActions *ActionsHeadInventory `json:"-"`
}

// ObserveRequiredCI performs bounded read-only collection for one exact target.
// Required context/app identity determines workflow fanout. Result-count movement
// permits one same-commit recollection within CIResults' original budgets. No
// job carry-forward, optional workflow walk or external mutation is performed.
func (g *GitHub) ObserveRequiredCI(ctx context.Context, target CITarget) (CIObservation, error) {
	return g.observeCI(ctx, target, target.BaseRef)
}

// ObserveCIGate assesses an explicitly selected branch's required checks against
// this PR's actual source candidate. The caller selects the gate; it never
// substitutes for the target's base branch, commit or test-merge verification.
func (g *GitHub) ObserveCIGate(ctx context.Context, target CITarget, gateRef string) (CIObservation, error) {
	return g.observeCI(ctx, target, gateRef)
}

func (g *GitHub) observeCI(ctx context.Context, target CITarget, gateRef string) (CIObservation, error) {
	return g.observeCIWithInitialAuthority(ctx, target, gateRef, nil, time.Time{})
}

func (g *GitHub) observeCIWithInitialAuthority(ctx context.Context, target CITarget, gateRef string, initial *RequiredCheckAuthority, started time.Time) (out CIObservation, err error) {
	if ctx == nil || !validRepositorySelector(target.Repository) || target.RepositoryID <= 0 || !validPullNumber(target.PullNumber) || !lowerCISHA(target.HeadSHA) || !lowerCISHA(target.BaseSHA) || !validRequiredBranch(target.BaseRef) || !validRequiredBranch(gateRef) {
		return CIObservation{}, invalidSelector()
	}
	out = CIObservation{Target: target, State: "unproven", Reason: "ci_authority_unproven", StartedAt: time.Now().UTC()}
	if !started.IsZero() {
		out.StartedAt = started
	}
	stage := "authority"
	defer func() {
		out.ObservedAt = time.Now().UTC()
		if err != nil {
			err = &ciCollectionError{stage: stage, cause: err}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, CIObservationTimeout)
	defer cancel()
	if initial == nil {
		out.Authority, err = g.RequiredCheckAuthority(ctx, target.Repository, gateRef)
	} else {
		out.Authority = *initial
		out.Authority.Checks = slices.Clone(initial.Checks)
	}
	if err != nil || out.Authority.State != "complete" || out.Authority.Checks == nil {
		return out, err
	}
	if len(out.Authority.Checks) > 128 {
		out.Reason = "ci_required_context_capacity"
		return out, nil
	}
	commits := make(map[string]githubCIMerge, 2)
	stage = "candidate"
	candidate, err := g.pullCICandidate(ctx, target.Repository, target.RepositoryID, target.PullNumber, target.HeadSHA, commits)
	out.Candidate, out.MergeState = &candidate, candidate.MergeState
	if err != nil {
		out.Reason = "ci_source_unavailable"
		return out, err
	}
	if !matchesCITarget(candidate, target) && !NonstrictHeadCICandidate(candidate, target, out.Authority) {
		out.Reason = "ci_target_unproven"
		// Keep a known candidate diagnostic visible in the compact result. A
		// mergeable PR can still expose a stale, unverified test merge.
		if candidate.State == "unproven" {
			switch candidate.Reason {
			case "ci_merge_identity_unproven", "ci_merge_base_mismatch", "ci_merge_pending", "ci_candidate_changed", "ci_target_changed":
				out.Reason = candidate.Reason
			}
		}
		return out, nil
	}
	if err = g.collectCIBody(ctx, candidate, &out, &stage); err != nil {
		return out, err
	}
	err = g.finishCIProof(ctx, gateRef, candidate, commits, &out, &stage)
	return out, err
}

// NonstrictHeadCICandidate permits only head CI under complete non-strict
// authority when a stable cached PR comparison trails the live base. It does
// not verify or select that stale test merge, or establish merge readiness.
func NonstrictHeadCICandidate(candidate CICandidate, target CITarget, authority RequiredCheckAuthority) bool {
	if authority.State != "complete" || authority.Reason != "" || authority.Strict || authority.Checks == nil || authority.ObservedAt.IsZero() || !strings.EqualFold(authority.Repository, target.Repository) || !validRequiredBranch(authority.Branch) ||
		candidate.State != "unproven" || candidate.Reason != "ci_merge_base_mismatch" || candidate.MergeState != "unproven" || candidate.MergeSHA != "" || candidate.MergeBaseParentSHA != "" || candidate.ObservedAt.IsZero() ||
		candidate.RepositoryID != target.RepositoryID || !strings.EqualFold(candidate.Repository, target.Repository) || candidate.PullNumber != target.PullNumber || candidate.HeadSHA != target.HeadSHA || candidate.BaseSHA != target.BaseSHA || candidate.BaseRef != target.BaseRef ||
		!validRepositorySelector(target.Repository) || target.RepositoryID <= 0 || !validPullNumber(target.PullNumber) || !lowerCISHA(target.HeadSHA) || !lowerCISHA(target.BaseSHA) || !validRequiredBranch(target.BaseRef) {
		return false
	}
	m := candidate.MergeBaseMismatch
	return m != nil && lowerCISHA(m.PRBaseSHA) && lowerCISHA(m.LiveBaseSHA) && lowerCISHA(m.MergeSHA) && lowerCISHA(m.MergeBaseParentSHA) &&
		m.PRBaseSHA == candidate.BaseSHA && m.MergeBaseParentSHA == m.PRBaseSHA && m.LiveBaseSHA != m.MergeBaseParentSHA && m.MergeBaseParentSHA != candidate.HeadSHA && m.MergeSHA != candidate.HeadSHA && m.MergeSHA != m.MergeBaseParentSHA
}

func matchesCITarget(candidate CICandidate, target CITarget) bool {
	return candidate.State == "complete" && candidate.RepositoryID == target.RepositoryID && strings.EqualFold(candidate.Repository, target.Repository) && candidate.PullNumber == target.PullNumber && candidate.HeadSHA == target.HeadSHA && candidate.BaseSHA == target.BaseSHA && candidate.BaseRef == target.BaseRef
}

func relevantCIWorkflows(required []RequiredCheck, results CIResultInventory, actions ActionsHeadInventory) map[int64]bool {
	suites := make(map[int64]int64, len(actions.Runs))
	for _, run := range actions.Runs {
		suites[run.CheckSuiteID] = run.WorkflowID
	}
	relevant := map[int64]bool{}
	for _, check := range results.Checks {
		for _, need := range required {
			if check.Name == need.Context && (need.AppID == 0 || check.AppID == need.AppID) && suites[check.SuiteID] > 0 {
				relevant[suites[check.SuiteID]] = true
			}
		}
	}
	return relevant
}

func sameCIAuthority(a, b RequiredCheckAuthority) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
func sameCICandidate(a, b CICandidate) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
func sameCIResults(a, b CIResultInventory) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	a.Checks, b.Checks = slices.Clone(a.Checks), slices.Clone(b.Checks)
	slices.SortFunc(a.Checks, func(a, b CICheckRun) int { return compareCIID(a.ID, b.ID) })
	slices.SortFunc(b.Checks, func(a, b CICheckRun) int { return compareCIID(a.ID, b.ID) })
	return reflect.DeepEqual(a, b)
}
func sameCIActions(a, b ActionsHeadInventory) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	a.Runs, b.Runs = slices.Clone(a.Runs), slices.Clone(b.Runs)
	slices.SortFunc(a.Runs, func(a, b ActionsHeadRun) int { return compareCIID(a.ID, b.ID) })
	slices.SortFunc(b.Runs, func(a, b ActionsHeadRun) int { return compareCIID(a.ID, b.ID) })
	return reflect.DeepEqual(a, b)
}
func compareCIID(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
