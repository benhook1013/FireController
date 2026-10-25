package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// FrontSnapshot is an as-of local explanation, never review/CI/merge authority.
type FrontSnapshot struct {
	ProjectID             string         `json:"project_id"`
	Configured            bool           `json:"configured"`
	RepositoryID          int64          `json:"repository_id"`
	ConfigurationRevision int64          `json:"configuration_revision"`
	PolicyRevision        int64          `json:"policy_revision"`
	SnapshotID            string         `json:"snapshot_id,omitempty"`
	Queue                 []int64        `json:"queue"`
	Channels              []FrontChannel `json:"channels"`
}
type FrontChannel struct {
	Channel          string `json:"channel"`
	State            string `json:"state"`
	Reason           string `json:"reason,omitempty"`
	PullNumber       int64  `json:"pull_number,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
	BaseSHA          string `json:"base_sha,omitempty"`
	BaseRef          string `json:"base_ref,omitempty"`
	SourceObservedAt string `json:"source_observed_at,omitempty"`
	CheckedAt        string `json:"checked_at,omitempty"`
}

// ReadFronts reuses the admission evaluator against current local policy and
// obligations in one authorized transaction. It never refreshes provider facts.
func (e *Engine) ReadFronts(ctx context.Context, scope core.Scope) (FrontSnapshot, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(scope.ProjectID) {
		return FrontSnapshot{}, invalid()
	}
	out := FrontSnapshot{ProjectID: scope.ProjectID, Queue: []int64{}, Channels: []FrontChannel{}}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&out.RepositoryID); err != nil {
			return err
		}
		var hasWorkspace bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_workspace')`).Scan(&hasWorkspace); err != nil {
			return err
		}
		if hasWorkspace {
			err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM review_workspace WHERE project_id=?`, scope.ProjectID).Scan(&out.SnapshotID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
				return nil
			}
			return err
		}
		if validateConfigurationShape(c) != nil {
			return runUnavailable()
		}
		policy, err := readPolicy(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		out.Configured = true
		out.ConfigurationRevision = revision
		out.PolicyRevision = policy.Revision
		out.Queue = append(out.Queue, c.Queue...)
		var recoveryHeld bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&recoveryHeld); err != nil {
			return err
		}
		var localPulls map[int64]localFrontPull
		for _, channel := range c.Channels {
			value := FrontChannel{Channel: channel, State: "unknown", Reason: "source_frame_unavailable"}
			frame, found, err := readFrontFrame(ctx, tx, scope.ProjectID, channel)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Code == "storage_unavailable" {
					value.Reason = "source_frame_invalid"
					out.Channels = append(out.Channels, value)
					continue
				}
				return err
			}
			if found {
				value.CheckedAt = frame.checkedAt
				value.SourceObservedAt = frame.observedAt
			}
			switch {
			case recoveryHeld:
				value.State = "held"
				value.Reason = "restore_recovery_held"
			case c.Paused:
				value.State = "held"
				value.Reason = "reviews_paused"
			case e.validateConfiguration(c) != nil:
				value.Reason = "capability_unavailable"
			case policy.Revision < 1:
				value.Reason = "policy_unconfigured"
			case !found:
			case frame.repository != out.RepositoryID:
				value.Reason = "repository_changed"
			case frame.configuration != revision:
				value.Reason = "configuration_changed"
			case frame.sourceReason != "":
				value.Reason = frame.sourceReason
			default:
				matches, visible, err := frontVisibleMatches(ctx, tx, scope.ProjectID, out.SnapshotID, frame.prefix)
				if err != nil {
					return err
				}
				if !matches {
					value.Reason = "visible_source_changed"
					break
				}
				result, err := e.evaluateFront(ctx, tx, Run{ProjectID: scope.ProjectID, Channel: channel}, out.RepositoryID, frame.plans(), frame.complete)
				if err != nil {
					return err
				}
				if result.state == "ready" && !visible[result.plan.Pull.Number] {
					result = evaluatedFront{state: "unknown", reason: "workspace_unavailable"}
				}
				value.State = result.state
				value.Reason = result.reason
				if result.plan != nil {
					p := result.plan.Pull
					value.PullNumber = p.Number
					value.HeadSHA = p.HeadSHA
					value.BaseSHA = p.BaseSHA
					value.BaseRef = p.BaseRef
					value.SourceObservedAt = result.plan.ObservedAt
				}
			}
			if localFrontFallback(value) && out.SnapshotID != "" {
				if localPulls == nil {
					localPulls, err = readLocalFrontPulls(ctx, tx, scope.ProjectID, out.SnapshotID, c.Queue)
					if err != nil {
						return err
					}
				}
				if local, found, err := localChannelFront(ctx, tx, scope.ProjectID, c.Queue, channel, localPulls); err != nil {
					return err
				} else if found {
					value = local
				}
			}
			out.Channels = append(out.Channels, value)
		}
		return nil
	})
	if err != nil {
		return FrontSnapshot{}, err
	}
	return out, nil
}

// Visibility is a local acquisition fact, not a demand to import old history.
func frontVisibleMatches(ctx context.Context, tx *sql.Tx, project, snapshot string, prefix []frontPull) (bool, map[int64]bool, error) {
	visible := make(map[int64]bool, len(prefix))
	var hasProjection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_current_pulls')`).Scan(&hasProjection); err != nil {
		return false, nil, err
	}
	for _, p := range prefix {
		if !hasProjection {
			if p.VisibleAtAcquisition {
				return false, nil, nil
			}
			continue
		}
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, project, p.Number).Scan(&retired); err != nil {
			return false, nil, err
		}
		if retired {
			return false, nil, nil
		}
		var state, head, base, ref string
		err := tx.QueryRowContext(ctx, `SELECT state,head_sha,base_sha,base_ref FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, project, snapshot, p.Number).Scan(&state, &head, &base, &ref)
		if errors.Is(err, sql.ErrNoRows) {
			if p.VisibleAtAcquisition {
				return false, nil, nil
			}
			continue
		}
		if err != nil {
			return false, nil, err
		}
		visible[p.Number] = true
		expected := p.State
		if p.Merged {
			expected = "merged"
		}
		if state != expected || head != p.HeadSHA || base != p.BaseSHA || ref != p.BaseRef {
			return false, nil, nil
		}
	}
	return true, visible, nil
}
