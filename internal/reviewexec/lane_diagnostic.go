package reviewexec

import (
	"context"
	"database/sql"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

// LaneDiagnostic reports local policy against explicitly retained source facts.
// It is not an admission preview: source freshness, automatic-intent ownership,
// provider capacity and dispatch authorization still require normal selection.
type LaneDiagnostic struct {
	ProjectID                   string                    `json:"project_id"`
	Channel                     string                    `json:"channel"`
	ConfigurationRevision       int64                     `json:"configuration_revision"`
	EvaluatedAt                 string                    `json:"evaluated_at"`
	Scope                       string                    `json:"scope"`
	SourceConfigurationRevision int64                     `json:"source_configuration_revision"`
	SourceCheckedAt             string                    `json:"source_checked_at,omitempty"`
	SourceComplete              bool                      `json:"source_complete"`
	SourceReason                string                    `json:"source_reason"`
	LaneReason                  string                    `json:"lane_reason"`
	Candidates                  []LaneCandidateDiagnostic `json:"candidates"`
}

type LaneCandidateDiagnostic struct {
	PullNumber         int64  `json:"pull_number"`
	HeadSHA            string `json:"head_sha,omitempty"`
	SourceObservedAt   string `json:"source_observed_at,omitempty"`
	ParentPullNumber   int64  `json:"parent_pull_number"`
	LocalState         string `json:"local_state"`
	LocalReason        string `json:"local_reason"`
	BlockingPullNumber int64  `json:"blocking_pull_number"`
	BlockingReason     string `json:"blocking_reason"`
}

func (e *Engine) ReadLaneDiagnostic(ctx context.Context, scope core.Scope, channel string) (LaneDiagnostic, error) {
	if !validChannel(channel) {
		return LaneDiagnostic{}, invalid()
	}
	out := LaneDiagnostic{ProjectID: scope.ProjectID, Channel: channel, Scope: "retained_source_local_policy", Candidates: []LaneCandidateDiagnostic{}}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		out.ConfigurationRevision, out.EvaluatedAt = revision, stamp()
		if c.Paused {
			out.LaneReason = "reviews_paused"
		} else if !slices.Contains(c.Channels, channel) {
			out.LaneReason = "review_not_configured"
		}
		policy, err := readPolicy(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		if slices.Contains(policy.Policy.PausedChannels, channel) {
			out.LaneReason = "reviews_paused"
		}
		frame, found, err := readFrontFrame(ctx, tx, scope.ProjectID, channel)
		if err != nil {
			return err
		}
		var repository int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
			return err
		}
		out.SourceReason = "source_unavailable"
		if found {
			if frame.repository != repository && out.LaneReason == "" {
				out.LaneReason = "repository_changed"
			}
			out.SourceConfigurationRevision, out.SourceCheckedAt = frame.configuration, frame.checkedAt
			out.SourceReason = frame.sourceReason
			switch {
			case frame.configuration != revision:
				out.SourceReason = "configuration_changed"
			case frame.repository != repository:
				out.SourceReason = "repository_changed"
			}
		}
		usable := found && frame.configuration == revision && frame.repository == repository
		out.SourceComplete = usable && frame.complete
		plans := frame.plans()
		byNumber := map[int64]*SourcePlan{}
		if usable {
			for i := range plans {
				byNumber[plans[i].Pull.Number] = &plans[i]
			}
		}
		// An empty selection identity deliberately does not impersonate an automatic
		// intent. This diagnostic describes local evidence/dependency facts only.
		r := Run{ProjectID: scope.ProjectID, Channel: channel}
		// Every local fact belongs to this same read transaction. Reuse ancestor
		// evaluations so a full queue of siblings does not repeat thousands of SQL
		// reads under the bounded Store.Read deadline. This cache never survives the
		// request and is not used by selection or admission.
		local := map[int64]evaluatedFront{}
		evaluate := func(plan *SourcePlan) (evaluatedFront, error) {
			if state, ok := local[plan.Pull.Number]; ok {
				return state, nil
			}
			state, err := e.evaluateFrontPlan(ctx, tx, r, repository, plan)
			if err == nil {
				local[plan.Pull.Number] = state
			}
			return state, err
		}
		for _, number := range c.Queue {
			item := LaneCandidateDiagnostic{PullNumber: number, LocalState: "unknown", LocalReason: "source_not_observed"}
			if plan := byNumber[number]; plan != nil {
				item.HeadSHA, item.SourceObservedAt, item.ParentPullNumber = plan.Pull.HeadSHA, plan.ObservedAt, plan.ParentPullNumber
				state, err := evaluateCandidateLocalState(plan, byNumber, evaluate)
				if err != nil {
					return err
				}
				item.LocalState, item.LocalReason = state.state, state.reason
				if globalFrontHold(state.reason) && out.LaneReason == "" {
					out.LaneReason = state.reason
				}
				if item.LocalState == "ready" {
					item.LocalState = "locally_ready"
				}
				item.BlockingPullNumber, item.BlockingReason = state.blockingPull, state.blockingReason
			}
			out.Candidates = append(out.Candidates, item)
		}
		return nil
	})
	if err != nil {
		return LaneDiagnostic{}, err
	}
	return out, nil
}
