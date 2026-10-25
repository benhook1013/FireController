package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const sourceCIMaxBytes = 8 << 20

const sourceCILatestQuery = `SELECT id FROM review_source_refreshes WHERE project_id=? AND pull_number=? AND state='completed' AND (include_ci=1 OR include_readiness=1) ORDER BY created_at DESC,id DESC LIMIT 1`

// Queue status reports collected CI independently of conflict-free merging.
func sourceCIQueueState(state string) string {
	switch state {
	case "satisfied":
		return "success"
	case "failed":
		return "failure"
	case "pending", "not_required":
		return state
	default:
		return "unknown"
	}
}

// SourceCISummary is an as-of CI observation, never permission to merge. Zero
// required with state unproven means unknown requirements, not absence.
type SourceCISummary struct {
	State             string `json:"state"`
	Reason            string `json:"reason"`
	Selection         string `json:"selection"`
	SelectedCommitSHA string `json:"selected_commit_sha"`
	MergeState        string `json:"merge_state"`
	ObservedAt        string `json:"observed_at"`
	Required          int64  `json:"required"`
	Satisfied         int64  `json:"satisfied"`
	Failed            int64  `json:"failed"`
	Pending           int64  `json:"pending"`
	Unproven          int64  `json:"unproven"`
}

type SourceCIAssessment struct {
	ProjectID             string            `json:"project_id"`
	RefreshID             string            `json:"refresh_id"`
	Source                string            `json:"source"`
	ConfigurationRevision int64             `json:"configuration_revision"`
	SourceObservedAt      string            `json:"source_observed_at"`
	EvaluatedAt           string            `json:"evaluated_at"`
	Target                SourceObservation `json:"target"`
	SourceCISummary
}

type sourceCIReader interface {
	ObserveRequiredCI(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error)
}

func sourceCITarget(o SourceObservation) reviewprovider.CITarget {
	return reviewprovider.CITarget{RepositoryID: o.RepositoryID, Repository: o.RepositoryName, PullNumber: o.PullNumber, HeadSHA: o.HeadSHA, BaseSHA: o.BaseSHA, BaseRef: o.BaseRef}
}
func sourcePlanObservation(plan SourcePlan) SourceObservation {
	return SourceObservation{RepositoryID: plan.Repository.ID, RepositoryName: plan.Repository.FullName, PullNumber: plan.Pull.Number, HeadSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, BaseRef: plan.Pull.BaseRef, ParentPullNumber: plan.ParentPullNumber, ParentSHA: plan.ParentSHA, ObservedAt: plan.ObservedAt}
}
func unavailableSourceCI(target reviewprovider.CITarget, started time.Time, reason string) *reviewprovider.CIObservation {
	return &reviewprovider.CIObservation{Target: target, StartedAt: started, ObservedAt: time.Now().UTC(), State: "unproven", Reason: reason}
}
func observeSourceCI(ctx context.Context, reader TargetReader, plan SourcePlan) *reviewprovider.CIObservation {
	source := sourcePlanObservation(plan)
	target := sourceCITarget(source)
	started := time.Now().UTC()
	ciReader, ok := reader.(sourceCIReader)
	if !ok {
		return unavailableSourceCI(target, started, "ci_reader_unavailable")
	}
	observation, err := ciReader.ObserveRequiredCI(ctx, target)
	if err != nil {
		return unavailableSourceCI(target, started, sourceCIErrorReason(ctx, err))
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return unavailableSourceCI(target, started, "ci_observation_unproven")
	}
	if len(body) > sourceCIMaxBytes {
		return unavailableSourceCI(target, started, "ci_observation_capacity")
	}
	var owned reviewprovider.CIObservation
	sourceAt, sourceTimeErr := time.Parse(time.RFC3339Nano, source.ObservedAt)
	// These checks concern adoption of newly collected evidence. Retained
	// observations remain readable if the local wall clock later moves back.
	if json.Unmarshal(body, &owned) != nil || sourceTimeErr != nil || !refreshTime(source.ObservedAt) || owned.StartedAt.IsZero() || owned.ObservedAt.IsZero() || owned.StartedAt.Before(sourceAt) || owned.ObservedAt.Before(owned.StartedAt) || owned.ObservedAt.After(time.Now().Add(5*time.Second)) {
		return unavailableSourceCI(target, started, "ci_observation_unproven")
	}
	// The collector stops before result collection at this supported boundary.
	// Preserve only the scoped capacity outcome, never its oversized inventory.
	if owned.Target == target && owned.State == "unproven" && owned.Reason == "ci_required_context_capacity" && len(owned.Authority.Checks) > 128 && owned.Authority.State == "complete" && owned.Authority.Reason == "" && owned.Authority.Repository == target.Repository && owned.Authority.Branch == target.BaseRef && !owned.Authority.ObservedAt.IsZero() {
		return unavailableSourceCI(target, started, "ci_required_context_capacity")
	}
	if !validSourceCI(owned, source) {
		return unavailableSourceCI(target, started, "ci_observation_unproven")
	}
	return &owned
}

// Retain only known safe classes. Adapter messages, URLs and partial inventories
// are not evidence of a complete CI collection and never enter this outcome.
func sourceCIErrorReason(ctx context.Context, err error) string {
	reason := sourceCIErrorClass(ctx, err)
	if stage := reviewprovider.CICollectionStage(err); stage != "" {
		return reason + "_" + stage
	}
	return reason
}

func sourceCIErrorClass(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "ci_collection_deadline"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "ci_collection_canceled"
	}
	var failure *reviewprovider.Error
	if errors.As(err, &failure) && failure != nil {
		switch failure.Code {
		case "provider_rate_limited", "provider_unauthorized", "provider_not_found", "provider_invalid_response", "provider_observation_changed":
			return "ci_" + failure.Code
		case "provider_unavailable":
			switch {
			case failure.Status == 408:
				return "ci_provider_timeout"
			case failure.Status >= 500 && failure.Status <= 599:
				return "ci_provider_server_error"
			case failure.Status >= 300 && failure.Status <= 399:
				return "ci_provider_redirect"
			}
		}
	}
	return "ci_provider_unavailable"
}

func decodeSourceCI(body string, r *sourceRefresh) error {
	if len(body) > sourceCIMaxBytes {
		return refreshStorageError()
	}
	if body == "null" {
		if (r.IncludeCI || r.IncludeReadiness) && r.State == "completed" {
			return refreshStorageError()
		}
		return nil
	}
	if !(r.IncludeCI || r.IncludeReadiness) || r.State != "completed" || r.Observation == nil {
		return refreshStorageError()
	}
	var observation reviewprovider.CIObservation
	if (strictjson.Validate([]byte(body)) != nil || json.Unmarshal([]byte(body), &observation) != nil) || !validSourceCIWithRetainedAssessment(observation, *r.Observation, true) {
		return refreshStorageError()
	}
	// Canonical server-owned JSON also rejects removed required keys and nulls
	// that would otherwise decode to zero values in this private retained record.
	encoded, err := json.Marshal(observation)
	if err != nil || string(encoded) != body {
		return refreshStorageError()
	}
	r.ciObservation = &observation
	summary := summarizeSourceCI(observation)
	r.CI = &summary
	return nil
}

func validSourceCI(o reviewprovider.CIObservation, source SourceObservation) bool {
	return validSourceCIWithRetainedAssessment(o, source, false)
}

func validSourceCIWithRetainedAssessment(o reviewprovider.CIObservation, source SourceObservation, allowLegacyOriginHold bool) bool {
	if !refreshTime(source.ObservedAt) || o.Target != sourceCITarget(source) || o.StartedAt.IsZero() || o.ObservedAt.IsZero() || o.Reason != "" && !identifier.MatchString(o.Reason) {
		return false
	}
	switch o.State {
	case "satisfied", "failed", "pending", "unproven", "not_required":
	default:
		return false
	}
	if o.State == "unproven" && o.Reason == "" || o.State == "satisfied" && o.Reason != "" || o.State == "not_required" && o.Reason != "" {
		return false
	}
	switch o.MergeState {
	case "", "pending", "conflicting", "verified", "unproven":
	default:
		return false
	}
	switch o.Selection {
	case "":
		if o.SelectedCommitSHA != "" {
			return false
		}
	case "head":
		if o.SelectedCommitSHA != source.HeadSHA {
			return false
		}
	case "test_merge":
		if o.Candidate == nil || o.Candidate.MergeState != "verified" || o.Candidate.MergeSHA != o.SelectedCommitSHA || !sha.MatchString(o.SelectedCommitSHA) {
			return false
		}
	default:
		return false
	}
	if len(o.Authority.Checks) > 128 || len(o.Attempts) > 16 {
		return false
	}
	if o.Authority.State == "complete" && (!strings.EqualFold(o.Authority.Repository, source.RepositoryName) || o.Authority.Branch != source.BaseRef || o.Authority.ObservedAt.IsZero() || o.Authority.Checks == nil) {
		return false
	}
	if o.State != "unproven" {
		if o.Candidate == nil || o.Candidate.State != "complete" || o.Candidate.RepositoryID != source.RepositoryID || !strings.EqualFold(o.Candidate.Repository, source.RepositoryName) || o.Candidate.PullNumber != source.PullNumber || o.Candidate.HeadSHA != source.HeadSHA || o.Candidate.BaseSHA != source.BaseSHA || o.Candidate.BaseRef != source.BaseRef || o.MergeState != o.Candidate.MergeState {
			return false
		}
	}
	if o.Results != nil && (!strings.EqualFold(o.Results.Repository, source.RepositoryName) || o.Results.HeadSHA != o.SelectedCommitSHA || o.Results.ObservedAt.IsZero() || len(o.Results.Checks)+len(o.Results.Statuses) > 10000) {
		return false
	}
	if o.Actions != nil && (!strings.EqualFold(o.Actions.Repository, source.RepositoryName) || o.Actions.RepositoryID != source.RepositoryID || o.Actions.HeadSHA != o.SelectedCommitSHA || o.Actions.ObservedAt.IsZero() || len(o.Actions.Runs) >= 1000) {
		return false
	}
	if o.TestMergeResults != nil && (o.Candidate == nil || !strings.EqualFold(o.TestMergeResults.Repository, source.RepositoryName) || o.TestMergeResults.HeadSHA != o.Candidate.MergeSHA || o.TestMergeResults.ObservedAt.IsZero() || len(o.TestMergeResults.Checks)+len(o.TestMergeResults.Statuses) > 10000) {
		return false
	}
	items := 0
	for _, attempt := range o.Attempts {
		items += len(attempt.Jobs)
		if items > 10000 {
			return false
		}
	}
	// Recompute the compact required-entry facts from retained typed inventories.
	// A later collector coherence check can downgrade a green assessment to
	// unproven; the original counts then remain informational only.
	if o.Assessment.State != "" {
		inventory := reviewprovider.CIResultInventory{Repository: source.RepositoryName, HeadSHA: source.HeadSHA}
		actions := reviewprovider.ActionsHeadInventory{}
		if o.Results != nil {
			inventory = *o.Results
		}
		if o.Actions != nil {
			actions = *o.Actions
		}
		assessment, err := reviewprovider.AssessRequiredCI(o.Authority, inventory, actions, o.Attempts)
		if err != nil {
			return false
		}
		matches := reflect.DeepEqual(assessment, o.Assessment)
		if !matches && allowLegacyOriginHold {
			matches = reviewprovider.ValidRetainedRequiredCIAssessment(o.Authority, inventory, actions, o.Attempts, o.Assessment)
		}
		if !matches {
			return false
		}
		if o.State != "unproven" && (o.State != o.Assessment.State || o.Reason != o.Assessment.Reason) {
			return false
		}
	} else if o.State != "unproven" {
		return false
	}
	if o.State == "not_required" {
		return o.Authority.State == "complete" && o.Authority.Checks != nil && len(o.Authority.Checks) == 0 && o.Selection == "" && o.SelectedCommitSHA == ""
	}
	if o.State != "unproven" && (o.Selection == "" || o.Results == nil || o.Actions == nil || o.Attempts == nil) {
		return false
	}
	return true
}

func summarizeSourceCI(o reviewprovider.CIObservation) SourceCISummary {
	s := SourceCISummary{State: o.State, Reason: o.Reason, Selection: o.Selection, SelectedCommitSHA: o.SelectedCommitSHA, MergeState: o.MergeState, ObservedAt: o.ObservedAt.UTC().Format(time.RFC3339Nano)}
	var unprovenReason string
	uniformReason := true
	for _, check := range o.Assessment.Checks {
		s.Required++
		switch check.State {
		case "satisfied":
			s.Satisfied++
		case "failed":
			s.Failed++
		case "pending":
			s.Pending++
		default:
			s.Unproven++
			if s.Unproven == 1 {
				unprovenReason = check.Reason
			} else if check.Reason != unprovenReason {
				uniformReason = false
			}
			if check.State != "unproven" || check.Reason == "" {
				uniformReason = false
			}
		}
	}
	if s.State == "unproven" && s.Reason == "ci_required_checks_unproven" && s.Unproven > 0 && uniformReason {
		s.Reason = unprovenReason
	}
	return s
}

// ReadSourceCIAssessment performs one authorized local snapshot read. Current
// source configuration and the latest completed comparison govern applicability;
// old CI counts remain informational when either binding has changed.
func (e *Engine) ReadSourceCIAssessment(ctx context.Context, scope core.Scope, number int64) (SourceCIAssessment, error) {
	if ctx == nil || e == nil || e.store == nil || number < 1 || number > 2147483647 {
		return SourceCIAssessment{}, invalid()
	}
	var result SourceCIAssessment
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, sourceCILatestQuery, scope.ProjectID, number).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Fail("reviews_ci_unavailable", "Explicitly refresh selected source with CI before assessing it.", 409)
		}
		if err != nil {
			return err
		}
		r, err := readSourceRefresh(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if r.CI == nil || r.Observation == nil {
			return refreshStorageError()
		}
		result = SourceCIAssessment{ProjectID: scope.ProjectID, RefreshID: r.ID, Source: "observed_ci", ConfigurationRevision: r.ConfigurationRevision, SourceObservedAt: r.Observation.ObservedAt, EvaluatedAt: stamp(), Target: *r.Observation, SourceCISummary: *r.CI}
		revision, reason, err := e.assessmentSourceBinding(ctx, tx, r)
		if err != nil {
			return err
		}
		result.ConfigurationRevision = revision
		if reason != "" {
			result.State, result.Reason = "held", reason
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
			if sourceCITarget(*latest.Observation) != sourceCITarget(*r.Observation) {
				result.State, result.Reason = "held", "source_comparison_changed"
			}
		}
		return nil
	})
	if err != nil {
		return SourceCIAssessment{}, err
	}
	return result, nil
}
