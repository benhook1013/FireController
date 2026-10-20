package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const sourceReviewMaxBytes = 8 << 20

const sourceReadinessLatestQuery = `SELECT id FROM review_source_refreshes WHERE project_id=? AND pull_number=? AND state='completed' AND include_readiness=1 ORDER BY created_at DESC,id DESC LIMIT 1`

// SourceReviewStateSummary exposes only bounded provider gate facts, never
// thread identities, comment text or permission to merge.
type SourceReviewStateSummary struct {
	State              string  `json:"state"`
	Reason             string  `json:"reason"`
	ObservedAt         string  `json:"observed_at"`
	PullState          string  `json:"pull_state"`
	Draft              bool    `json:"draft"`
	Mergeable          string  `json:"mergeable"`
	MergeStateStatus   string  `json:"merge_state_status"`
	ReviewDecision     *string `json:"review_decision"`
	TotalThreads       int64   `json:"total_threads"`
	UnresolvedCurrent  int64   `json:"unresolved_current"`
	UnresolvedOutdated int64   `json:"unresolved_outdated"`
}

type SourceReadinessAssessment struct {
	SourceObligationsPending int64                     `json:"source_obligations_pending,omitempty"`
	ProjectID                string                    `json:"project_id"`
	RefreshID                string                    `json:"refresh_id"`
	Source                   string                    `json:"source"`
	ConfigurationRevision    int64                     `json:"configuration_revision"`
	PolicyRevision           int64                     `json:"policy_revision"`
	SourceObservedAt         string                    `json:"source_observed_at"`
	EvaluatedAt              string                    `json:"evaluated_at"`
	Target                   SourceObservation         `json:"target"`
	State                    string                    `json:"state"`
	Reasons                  []string                  `json:"reasons"`
	Channels                 []PolicyAssessmentChannel `json:"channels"`
	CI                       SourceCISummary           `json:"ci"`
	ReviewState              SourceReviewStateSummary  `json:"review_state"`
	IncomingRoutesPending    int64                     `json:"incoming_routes_pending,omitempty"`
	IncomingRoutesCapacity   bool                      `json:"incoming_routes_capacity,omitempty"`
}

type sourceReviewReader interface {
	ObservePullReviewState(context.Context, reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error)
}

func unavailableSourceReview(target reviewprovider.CITarget, reason string) *reviewprovider.PullReviewObservation {
	return &reviewprovider.PullReviewObservation{Target: target, State: "unproven", Reason: reason, ObservedAt: time.Now().UTC()}
}
func observeSourceReviewState(ctx context.Context, reader TargetReader, plan SourcePlan) *reviewprovider.PullReviewObservation {
	target := sourceCITarget(sourcePlanObservation(plan))
	provider, ok := reader.(sourceReviewReader)
	if !ok {
		return unavailableSourceReview(target, "review_state_reader_unavailable")
	}
	observation, err := provider.ObservePullReviewState(ctx, target)
	if err != nil {
		return unavailableSourceReview(target, "review_state_provider_unavailable")
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return unavailableSourceReview(target, "review_state_unproven")
	}
	if len(body) > sourceReviewMaxBytes {
		return unavailableSourceReview(target, "review_state_capacity")
	}
	var owned reviewprovider.PullReviewObservation
	source := sourcePlanObservation(plan)
	sourceAt, sourceTimeErr := time.Parse(time.RFC3339Nano, source.ObservedAt)
	// Fresh collection must fall in its source observation interval. Retained
	// facts remain readable when the host wall clock later moves backwards.
	if json.Unmarshal(body, &owned) != nil || !validSourceReviewState(owned, source) || sourceTimeErr != nil || owned.ObservedAt.Before(sourceAt) || owned.ObservedAt.After(time.Now().Add(5*time.Second)) {
		return unavailableSourceReview(target, "review_state_unproven")
	}
	return &owned
}
func validSourceReviewState(o reviewprovider.PullReviewObservation, source SourceObservation) bool {
	if !refreshTime(source.ObservedAt) || o.Target != sourceCITarget(source) || o.ObservedAt.IsZero() || len(o.Threads) > 10000 {
		return false
	}
	if o.State != "complete" && o.State != "unproven" || o.State == "complete" && o.Reason != "" || o.State == "unproven" && !identifier.MatchString(o.Reason) {
		return false
	}
	// Unavailable collections retain their explicit unknown state, not fabricated
	// empty passing inventories or invented PR metadata.
	if o.State == "unproven" && o.PullState == "" {
		return !o.Draft && o.Mergeable == "" && o.MergeStateStatus == "" && o.ReviewDecision == nil && o.Threads == nil
	}
	if !slices.Contains([]string{"OPEN", "CLOSED", "MERGED"}, o.PullState) || !slices.Contains([]string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}, o.Mergeable) || !slices.Contains([]string{"BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE"}, o.MergeStateStatus) || o.Threads == nil {
		return false
	}
	if o.ReviewDecision != nil && !slices.Contains([]string{"APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"}, *o.ReviewDecision) {
		return false
	}
	seen := make(map[string]bool, len(o.Threads))
	for _, thread := range o.Threads {
		if thread.ID == "" || !safeText(thread.ID, 256) || seen[thread.ID] {
			return false
		}
		seen[thread.ID] = true
	}
	return true
}
func summarizeSourceReviewState(o reviewprovider.PullReviewObservation) SourceReviewStateSummary {
	if o.State != "complete" {
		return SourceReviewStateSummary{State: o.State, Reason: o.Reason, ObservedAt: o.ObservedAt.UTC().Format(time.RFC3339Nano)}
	}
	s := SourceReviewStateSummary{State: o.State, Reason: o.Reason, ObservedAt: o.ObservedAt.UTC().Format(time.RFC3339Nano), PullState: o.PullState, Draft: o.Draft, Mergeable: o.Mergeable, MergeStateStatus: o.MergeStateStatus, TotalThreads: int64(len(o.Threads))}
	if o.ReviewDecision != nil {
		decision := *o.ReviewDecision
		s.ReviewDecision = &decision
	}
	for _, thread := range o.Threads {
		if !thread.Resolved {
			if thread.Outdated {
				s.UnresolvedOutdated++
			} else {
				s.UnresolvedCurrent++
			}
		}
	}
	return s
}
func decodeSourceReviewState(body string, r *sourceRefresh) error {
	if len(body) > sourceReviewMaxBytes {
		return refreshStorageError()
	}
	if body == "null" {
		if r.IncludeReadiness && r.State == "completed" {
			return refreshStorageError()
		}
		return nil
	}
	if !r.IncludeReadiness || r.State != "completed" || r.Observation == nil {
		return refreshStorageError()
	}
	var observation reviewprovider.PullReviewObservation
	if strictjson.Validate([]byte(body)) != nil || json.Unmarshal([]byte(body), &observation) != nil || !validSourceReviewState(observation, *r.Observation) {
		return refreshStorageError()
	}
	canonical, err := json.Marshal(observation)
	if err != nil || string(canonical) != body {
		return refreshStorageError()
	}
	r.reviewObservation = &observation
	summary := summarizeSourceReviewState(observation)
	r.ReviewState = &summary
	return nil
}

// assessmentSourceBinding checks local applicability without inheriting the
// refreshing actor's permissions or treating execution pause as lost credit.
func (e *Engine) assessmentSourceBinding(ctx context.Context, tx *sql.Tx, r sourceRefresh) (int64, string, error) {
	c, revision, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
			return 0, "source_configuration_changed", nil
		}
		return 0, "", err
	}
	var repositoryID int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, r.ProjectID).Scan(&repositoryID); err != nil {
		return 0, "", err
	}
	retained, err := sourceConfiguration(ctx, tx, r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		return 0, "", err
	}
	cap, ok := e.capabilities[c.CapabilityID]
	if !sameSourceConfiguration(c, retained) || repositoryID != r.repositoryID || !ok || e.validateConfiguration(c) != nil || c.CapabilityID != r.capability || cap.Fingerprint != r.fingerprint || !slices.Contains(c.Queue, r.PullNumber) {
		return revision, "source_configuration_changed", nil
	}
	return revision, "", nil
}
func sameSourceComparison(a, b SourceObservation) bool {
	return sourceCITarget(a) == sourceCITarget(b) && a.ParentPullNumber == b.ParentPullNumber && a.ParentSHA == b.ParentSHA
}

// ReadSourceReadiness evaluates retained provider facts and current native
// obligations in one authorized SQL snapshot. It performs no provider work.
func (e *Engine) ReadSourceReadiness(ctx context.Context, scope core.Scope, number int64) (SourceReadinessAssessment, error) {
	if ctx == nil || e == nil || e.store == nil || number < 1 || number > 2147483647 {
		return SourceReadinessAssessment{}, invalid()
	}
	var out SourceReadinessAssessment
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, sourceReadinessLatestQuery, scope.ProjectID, number).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Fail("reviews_readiness_unavailable", "Explicitly refresh selected source with readiness before assessing it.", 409)
		}
		if err != nil {
			return err
		}
		r, err := readSourceRefresh(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if r.CI == nil || r.ReviewState == nil || r.Observation == nil {
			return refreshStorageError()
		}
		out = SourceReadinessAssessment{ProjectID: scope.ProjectID, RefreshID: r.ID, Source: "observed_comparison", SourceObservedAt: r.Observation.ObservedAt, EvaluatedAt: stamp(), Target: *r.Observation, State: "held", Reasons: []string{}, Channels: []PolicyAssessmentChannel{}, CI: *r.CI, ReviewState: *r.ReviewState}
		revision, reason, err := e.assessmentSourceBinding(ctx, tx, r)
		if err != nil {
			return err
		}
		out.ConfigurationRevision = revision
		policy, policyErr := readPolicy(ctx, tx, scope.ProjectID)
		if policyErr != nil {
			var failure *core.Error
			if !errors.As(policyErr, &failure) || failure.Code != "reviews_policy_unconfigured" {
				return policyErr
			}
		} else {
			out.PolicyRevision = policy.Revision
		}
		if reason != "" {
			out.Reasons = append(out.Reasons, reason)
			return nil
		}
		var latestID string
		if err = tx.QueryRowContext(ctx, `SELECT id FROM review_source_refreshes WHERE project_id=? AND pull_number=? AND state='completed' ORDER BY created_at DESC,id DESC LIMIT 1`, scope.ProjectID, number).Scan(&latestID); err != nil {
			return err
		}
		if latestID != r.ID {
			latest, err := readSourceRefreshMetadata(ctx, tx, scope.ProjectID, latestID)
			if err != nil {
				return err
			}
			if !sameSourceComparison(*latest.Observation, *r.Observation) {
				out.Reasons = append(out.Reasons, "source_comparison_changed")
				return nil
			}
		}
		if policyErr != nil {
			out.State = "unconfigured"
			out.Reasons = append(out.Reasons, "policy_unconfigured")
			return nil
		}
		out.IncomingRoutesPending, out.IncomingRoutesCapacity, err = incomingRoutePolicyFacts(ctx, tx, scope.ProjectID, number)
		if err != nil {
			return err
		}
		if out.IncomingRoutesCapacity {
			out.Reasons = append(out.Reasons, "routed_findings_capacity")
		} else if out.IncomingRoutesPending > 0 {
			out.Reasons = append(out.Reasons, "routed_findings_pending")
		}
		out.SourceObligationsPending, err = sourceObligationCount(ctx, tx, scope.ProjectID, number)
		if err != nil {
			return err
		}
		if out.SourceObligationsPending > 0 {
			out.Reasons = append(out.Reasons, "source_obligations_pending")
		}
		anchor := Anchor{RepositoryID: r.repositoryID, PullNumber: number, CandidateSHA: r.Observation.HeadSHA, BaseSHA: r.Observation.BaseSHA, BaseRef: r.Observation.BaseRef, ParentPullNumber: r.Observation.ParentPullNumber, ParentSHA: r.Observation.ParentSHA}
		missing, pending, roundsPending := false, false, false
		for _, channel := range policy.Policy.RequiredChannels {
			state, err := comparison(ctx, tx, scope.ProjectID, r.repositoryID, number, anchor, channel)
			if err != nil {
				return err
			}
			out.Channels = append(out.Channels, PolicyAssessmentChannel{Channel: channel, Covered: state.Covered, Pending: state.Pending, RoundsConfigured: state.RoundsConfigured, RoundsComplete: state.RoundsComplete})
			missing = missing || !state.Covered
			pending = pending || state.Pending
			roundsPending = roundsPending || state.RoundsConfigured && !state.RoundsComplete
		}
		if missing {
			out.Reasons = append(out.Reasons, "review_coverage_missing")
		}
		if pending {
			out.Reasons = append(out.Reasons, "review_evidence_pending")
		}
		if roundsPending {
			out.Reasons = append(out.Reasons, "review_rounds_pending")
		}
		switch out.CI.State {
		case "satisfied", "not_required":
		case "failed":
			out.Reasons = append(out.Reasons, "ci_failed")
		case "pending":
			out.Reasons = append(out.Reasons, "ci_pending")
		default:
			out.Reasons = append(out.Reasons, "ci_unproven")
		}
		if out.CI.MergeState != "verified" {
			out.Reasons = append(out.Reasons, "ci_merge_state_unproven")
		}
		review := out.ReviewState
		if review.State != "complete" {
			out.Reasons = append(out.Reasons, "review_state_unproven")
		} else {
			if review.PullState != "OPEN" {
				out.Reasons = append(out.Reasons, "pull_not_open")
			}
			if review.Draft {
				out.Reasons = append(out.Reasons, "pull_draft")
			}
			if review.Mergeable != "MERGEABLE" {
				out.Reasons = append(out.Reasons, "mergeability_unproven")
			}
			if review.MergeStateStatus != "CLEAN" {
				out.Reasons = append(out.Reasons, "merge_state_not_clean")
			}
			if review.ReviewDecision != nil {
				switch *review.ReviewDecision {
				case "CHANGES_REQUESTED":
					out.Reasons = append(out.Reasons, "review_changes_requested")
				case "REVIEW_REQUIRED":
					out.Reasons = append(out.Reasons, "review_required")
				}
			}
			if review.UnresolvedCurrent > 0 {
				out.Reasons = append(out.Reasons, "unresolved_current_threads")
			}
			if review.UnresolvedOutdated > 0 {
				out.Reasons = append(out.Reasons, "unresolved_outdated_threads")
			}
		}
		if len(out.Reasons) == 0 {
			out.State = "ready_as_observed"
		}
		return nil
	})
	if err != nil {
		return SourceReadinessAssessment{}, err
	}
	return out, nil
}
