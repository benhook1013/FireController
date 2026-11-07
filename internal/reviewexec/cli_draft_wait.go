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
	if len(e.draftProbeNext) >= 256 {
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
	driver, ok := drivers[execution.Configuration.CapabilityID].(interface{ SourceReader() TargetReader })
	if !ok || driver.SourceReader() == nil {
		return !waiting, false, false, nil
	}
	if !e.claimDraftProbe(o.ID) {
		return false, false, false, nil
	}
	reader := driver.SourceReader()
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	repo, err := reader.Repository(readCtx, execution.RepositoryID)
	if err != nil {
		return !waiting, false, true, nil
	}
	if repo.ID != execution.RepositoryID || !repository.MatchString(repo.FullName) {
		if !waiting {
			return true, false, true, nil
		}
		return false, false, true, e.recordCLIDraftDecision(ctx, o, "repository_changed")
	}
	pull, err := reader.Pull(readCtx, repo.FullName, o.PullNumber)
	if err != nil {
		return !waiting, false, true, nil
	}
	if !sameTarget(pull, execution.RepositoryID, o.PullNumber) || pull.HeadSHA != o.HeadSHA || pull.State != "open" || pull.Merged {
		if !waiting {
			return true, false, true, nil
		}
		return false, false, true, e.recordCLIDraftDecision(ctx, o, "target_changed")
	}
	if pull.IsDraft == nil && waiting {
		return false, false, false, nil
	}
	if pull.IsDraft != nil && *pull.IsDraft {
		if waiting {
			return false, false, false, nil
		}
		return false, false, true, e.recordCLIDraftDecision(ctx, o, cliDraftWait)
	}
	return true, waiting, true, nil
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
			return holdOperation(ctx, tx, current, reason, at)
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
