package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const (
	transientCLISourceHold = "preparation_source_resolution_provider_transient"
	legacyCLISourceHold    = "preparation_source_resolution_provider_unavailable"
	transientSourceDelay   = 30 * time.Second
)

func transientCLISourceReason(reason string) bool {
	return reason == transientCLISourceHold || reason == legacyCLISourceHold
}

// A terminal preparation can be reconsidered once, under the original
// selection. The retained front nominates work. The driver must still read
// fresh source and pass normal admission and dispatch guards.
func (e *Engine) automaticTransientCLIRetry(ctx context.Context, tx *sql.Tx, project string, c Configuration) (*Operation, error) {
	queue, err := json.Marshal(c.Queue)
	if err != nil {
		return nil, err
	}
	now := e.sampleExecutionTime()
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id,s.id,a.pull_basis_sha256 FROM json_each(?) q
		CROSS JOIN review_operations o ON o.project_id=? AND o.pull_number=q.value AND o.channel='cli'
		JOIN review_execution_attempts current ON current.id=o.attempt_id AND current.operation_id=o.id
		JOIN review_automatic_selections a ON a.project_id=o.project_id AND a.pull_number=o.pull_number
		JOIN review_selections s ON s.id=a.selection_id AND s.project_id=o.project_id AND s.operation_id=o.id
		WHERE o.state='held' AND o.reason IN (?,?)
		AND current.state='held' AND current.revision=2 AND current.anchor_json='null'
		AND current.intent='' AND current.external_id=''
		AND NOT EXISTS(SELECT 1 FROM review_cli_preflight p WHERE p.attempt_id=current.id)
		AND julianday(o.updated_at)<=julianday(?)
		AND NOT EXISTS(SELECT 1 FROM review_operations newer WHERE newer.project_id=o.project_id
			AND newer.pull_number=o.pull_number AND newer.channel=o.channel AND newer.id<>o.id
			AND julianday(newer.created_at)>=julianday(o.created_at))
		AND NOT EXISTS(SELECT 1 FROM review_execution_attempts old
			JOIN review_execution_transitions t ON t.attempt_id=old.id AND t.revision=2
			WHERE old.operation_id=o.id AND old.id<>o.attempt_id AND t.reason IN (?,?))
		ORDER BY q.key`, string(queue), project, transientCLISourceHold, legacyCLISourceHold,
		now.Add(-transientSourceDelay).UTC().Format(time.RFC3339Nano), transientCLISourceHold, legacyCLISourceHold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct{ operation, selection, basis string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.operation, &item.selection, &item.basis); err != nil {
			return nil, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil || len(candidates) == 0 {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	frame, found, err := readFrontFrame(ctx, tx, project, "cli")
	if err != nil || !found || frame.configuration != *c.ExpectedRevision+1 {
		return nil, err
	}
	plans := frame.plans()
	for _, candidate := range candidates {
		o, err := readOperationBase(ctx, tx, project, candidate.operation)
		if err != nil {
			return nil, err
		}
		heldAt, valid := attemptRecordedTime(o.UpdatedAt)
		if !valid || now.Before(heldAt.Add(transientSourceDelay)) || o.Wait != nil || o.Revision == math.MaxInt64 {
			continue
		}
		var selected *SourcePlan
		for i := range plans {
			if plans[i].Pull.Number == o.PullNumber && plans[i].Pull.HeadSHA == o.HeadSHA {
				selected = &plans[i]
				break
			}
		}
		if selected == nil || selected.SourceReason != "" {
			continue
		}
		if o.Reason == legacyCLISourceHold {
			// Older clients retained a generic provider error. Require
			// affirmative evidence of a successful read of the same source.
			ok, err := legacyCLISourceRecovered(ctx, tx, o, frame, *selected, heldAt)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		basis, err := automaticPullBasisChannel(ctx, tx, project, o.PullNumber, o.Channel)
		if err != nil {
			return nil, err
		}
		if basis != candidate.basis {
			continue // Normal selection handles a new comparison or changed controls.
		}
		current, err := e.currentExecutionContext(ctx, tx, o)
		if err != nil {
			var refusal *core.Error
			if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
				continue
			}
			return nil, err
		}
		if current.RepositoryID != frame.repository {
			continue
		}
		run, err := readRun(ctx, tx, project, candidate.selection)
		if err != nil {
			return nil, err
		}
		front, _, err := e.runFront(ctx, tx, run, frame.repository, plans)
		if err != nil || front != o.PullNumber {
			return nil, err
		}
		a, err := readAttempt(ctx, tx, project, o.AttemptID)
		if err != nil {
			return nil, err
		}
		detached := o
		detached.AttemptID = ""
		old, unused, err := historicalUnusedPreparationOperation(ctx, tx, detached, a, true)
		if err != nil || !unused || old.Reason != o.Reason {
			return nil, err
		}
		return &o, nil
	}
	return nil, nil
}

func legacyCLISourceRecovered(ctx context.Context, tx *sql.Tx, o Operation, f frontFrame, plan SourcePlan, heldAt time.Time) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM review_source_refreshes WHERE project_id=? AND repository_id=?
		AND pull_number=? AND head_sha=? AND configuration_revision=? AND capability_fingerprint=?
		AND state='completed' AND julianday(updated_at)>=julianday(?) ORDER BY julianday(updated_at) DESC LIMIT 1`,
		o.ProjectID, f.repository, o.PullNumber, o.HeadSHA, f.configuration, o.CapabilityFingerprint, o.UpdatedAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	refresh, err := readSourceRefreshMetadata(ctx, tx, o.ProjectID, id)
	if err != nil {
		return false, err
	}
	source := refresh.Observation
	observed, err := time.Parse(time.RFC3339Nano, source.ObservedAt)
	return err == nil && observed.After(heldAt) && source.BaseSHA == plan.Pull.BaseSHA &&
		source.BaseRef == plan.Pull.BaseRef && source.ParentPullNumber == plan.ParentPullNumber && source.ParentSHA == plan.ParentSHA, nil
}

func (e *Engine) requeueTransientCLISource(ctx context.Context, tx *sql.Tx, o Operation) error {
	at, err := attemptRecordingStamp(ctx, tx, o, nil, e.sampleExecutionTime())
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason='',attempt_id='',revision=revision+1,updated_at=? WHERE id=?`, at, o.ID); err != nil {
		return err
	}
	scope := core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}
	return core.AppendEvent(ctx, tx, scope, "review.operation_updated", o.ID, o.Revision+1,
		map[string]string{"action": "retry_transient_source", "attempt_id": o.AttemptID})
}
