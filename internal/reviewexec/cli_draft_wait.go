package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const cliDraftWait = "cli_draft_wait"
const cliDraftProbeInterval = 30 * time.Second
const cliDraftProbeCapacity = 256

// The wait itself is durable. Only the read throttle is process-local, so a
// restart can observe readiness again without creating a new review request.
func (e *Engine) claimDraftProbe(id string) bool {
	now := e.sampleExecutionTime()
	e.draftProbeMu.Lock()
	defer e.draftProbeMu.Unlock()
	if e.draftProbeNext[id].After(now) {
		return false
	}
	for key, next := range e.draftProbeNext {
		if !next.After(now) {
			delete(e.draftProbeNext, key)
		}
	}
	if len(e.draftProbeNext) >= cliDraftProbeCapacity {
		return false
	}
	if e.draftProbeNext == nil {
		e.draftProbeNext = make(map[string]time.Time)
	}
	e.draftProbeNext[id] = now.Add(cliDraftProbeInterval)
	return true
}

// This check only decides whether the same explicit request may reach normal
// preparation. It does not establish an anchor, allowance or permission to
// dispatch. Runtime preparation and final admission still make fresh checks.
func (e *Engine) checkCLIDraft(ctx context.Context, o Operation, drivers map[string]Driver) (proceed, ready, explicit bool, err error) {
	waiting := o.Reason == cliDraftWait
	if o.Channel != "cli" {
		return true, false, false, nil
	}
	var execution ExecutionContext
	var selected bool
	err = e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readOperationBase(ctx, tx, o.ProjectID, o.ID)
		if err != nil {
			return err
		}
		if current.State != "queued" || current.Revision != o.Revision {
			return conflict()
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND operation_id=?)`, o.ProjectID, o.ID).Scan(&selected); err != nil || selected {
			return err
		}
		execution, err = e.currentExecutionContext(ctx, tx, current)
		return err
	})
	if err != nil {
		var refusal *core.Error
		if errors.As(err, &refusal) {
			if refusal.Code == "overloaded" {
				return false, false, false, nil
			}
			if refusal.Status >= 400 && refusal.Status < 500 {
				// Let the normal claim settle a local refusal. A draft wait
				// cannot acquire a reservation without a positive source check.
				return true, false, false, nil
			}
		}
		return false, false, false, err
	}
	if selected {
		return true, false, false, nil
	}
	registered := drivers[execution.Configuration.CapabilityID]
	if registered == nil {
		if waiting {
			// A missing driver cannot establish Draft readiness. Settle the
			// local capability hold without inventing readiness or an attempt.
			return false, false, false, e.recordCLIDraftDecision(ctx, o, "capability_unavailable")
		}
		return true, false, false, nil
	}
	driver, ok := registered.(interface{ SourceReader() TargetReader })
	if !ok {
		return !waiting, false, false, nil
	}
	reader := driver.SourceReader()
	if reader == nil {
		return !waiting, false, false, nil
	}
	if !waiting {
		// Ordinary preparation already refreshes this exact source and retains
		// pull_draft as a wait. Do not duplicate those provider reads or spend
		// a draft-wait probe slot before the preparation path gets its turn.
		return true, false, true, nil
	}
	if !e.claimDraftProbe(o.ID) {
		return false, false, true, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	repo, err := reader.Repository(readCtx, execution.RepositoryID)
	if err != nil {
		return false, false, true, nil
	}
	if repo.ID != execution.RepositoryID || !repository.MatchString(repo.FullName) {
		return false, false, true, e.recordCLIDraftDecision(ctx, o, "repository_changed")
	}
	pull, err := reader.Pull(readCtx, repo.FullName, o.PullNumber)
	if err != nil {
		return false, false, true, nil
	}
	if !sameTarget(pull, execution.RepositoryID, o.PullNumber) || pull.HeadSHA != o.HeadSHA || pull.State != "open" || pull.Merged {
		reason := "target_changed"
		if obsolete := obsoleteTargetReason(pull, execution.RepositoryID, o.PullNumber, o.HeadSHA); obsolete != "" {
			reason = obsolete
		}
		return false, false, true, e.recordCLIDraftDecision(ctx, o, reason)
	}
	if pull.IsDraft == nil {
		return false, false, false, nil
	}
	if *pull.IsDraft {
		return false, false, false, nil
	}
	return true, true, true, nil
}

func (e *Engine) recordCLIDraftDecision(ctx context.Context, o Operation, reason string) error {
	changed := false
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readOperationBase(ctx, tx, o.ProjectID, o.ID)
		if err != nil || current.State != "queued" || current.Revision != o.Revision {
			return err
		}
		if current.Channel != "cli" || current.AttemptID != "" || current.Revision == math.MaxInt64 {
			return conflict()
		}
		at, err := attemptRecordingStamp(ctx, tx, current, nil, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		if reason != cliDraftWait {
			changed = true
			return e.holdOperation(ctx, tx, current, reason, at)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET reason=?,revision=revision+1,updated_at=? WHERE id=?`, reason, at, o.ID); err != nil {
			return err
		}
		changed = true
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{})
	})
	if err == nil && changed {
		e.notifyOperation(o.ID)
	}
	return err
}

// historicalCLIDraftOperation identifies an unused preparation detached when
// GitHub returned the PR to Draft. It supplies attribution for history only.
func historicalCLIDraftOperation(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (Operation, bool, error) {
	var reason string
	if err := q.QueryRowContext(ctx, `SELECT reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&reason); err != nil {
		return Operation{}, false, err
	}
	if reason != cliDraftWait {
		return Operation{}, false, nil
	}
	if o.Channel != "cli" || a.OperationID != o.ID || a.ProjectID != o.ProjectID || a.ID == o.AttemptID || a.State != "held" || a.Revision != 2 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
		return Operation{}, false, waitUnavailable()
	}
	var startedAt, heldAt string
	var occupied bool
	err := q.QueryRowContext(ctx, `SELECT first.created_at, last.created_at,
		EXISTS(SELECT 1 FROM review_execution_transitions WHERE attempt_id=first.attempt_id AND revision NOT IN (1,2))
		OR EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=first.attempt_id)
		OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=first.attempt_id)
		OR EXISTS(SELECT 1 FROM review_cli_preflight WHERE attempt_id=first.attempt_id)
		FROM review_execution_transitions first
		JOIN review_execution_transitions last ON last.attempt_id=first.attempt_id AND last.revision=2
		WHERE first.attempt_id=? AND first.revision=1 AND first.state='preparing' AND first.reason='preparing'
		AND last.state='held' AND first.external_id='' AND last.external_id=''`, a.ID).Scan(&startedAt, &heldAt, &occupied)
	started, validStart := attemptRecordedTime(startedAt)
	held, validHold := attemptRecordedTime(heldAt)
	if err != nil || occupied || !validStart || !validHold || held.Before(started) {
		return Operation{}, false, waitUnavailable()
	}
	o.AttemptID, o.State, o.Reason, o.Wait = a.ID, "held", cliDraftWait, nil
	return o, true, nil
}
