package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

// StartAttempt reserves both the external target and the provider account.
// It does no provider work; preparing has not authorized an external action.
func (e *Engine) StartAttempt(ctx context.Context, project, id string) (Attempt, error) {
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if o.State != "queued" {
			return conflict()
		}
		c, revision, err := readConfiguration(ctx, tx, project)
		if err != nil {
			return err
		}
		if c.Paused || revision != o.ConfigurationRevision {
			return holdOperation(ctx, tx, o, "configuration_changed")
		}
		capability, ok := e.capabilities[c.CapabilityID]
		if !ok || e.validateConfiguration(c) != nil {
			return holdOperation(ctx, tx, o, "capability_unavailable")
		}
		if o.CapabilityFingerprint != capability.Fingerprint {
			return holdOperation(ctx, tx, o, "capability_changed")
		}
		allowed, err := actorAllowed(ctx, tx, project, o.ActorID, o.WorkerID)
		if err != nil {
			return err
		}
		if !allowed {
			return holdOperation(ctx, tx, o, "authorization_changed")
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		var busy int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations WHERE provider=? AND action_class=? AND (account=? OR (repository_id=? AND pull_number=?))`, capability.Provider, o.Channel, capability.Account, repositoryID, o.PullNumber).Scan(&busy); err != nil {
			return err
		}
		if busy != 0 {
			return core.Fail("review_busy", "A review reservation is already occupied; no external action was started.", 409)
		}
		result = Attempt{ID: core.NewID("review_attempt"), OperationID: o.ID, ProjectID: project, Provider: capability.Provider, Account: capability.Account, State: "preparing", Revision: 1}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) VALUES(?,?,?,?,?,'preparing',1,'null','','')`, result.ID, o.ID, project, result.Provider, result.Account); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES(?,?,?,?,?,?)`, result.Provider, repositoryID, o.PullNumber, o.Channel, result.Account, result.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='preparing',attempt_id=?,revision=revision+1,updated_at=? WHERE id=?`, result.ID, stamp(), o.ID); err != nil {
			return err
		}
		return transitionRow(ctx, tx, result, "preparing")
	})
	return result, err
}

func holdOperation(ctx context.Context, tx *sql.Tx, o Operation, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason=?,revision=revision+1,updated_at=? WHERE id=?`, reason, stamp(), o.ID)
	return err
}

func readAttempt(ctx context.Context, tx *sql.Tx, project, id string) (Attempt, error) {
	var a Attempt
	var anchor string
	err := tx.QueryRowContext(ctx, `SELECT id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE project_id=? AND id=?`, project, id).Scan(&a.ID, &a.OperationID, &a.ProjectID, &a.Provider, &a.Account, &a.State, &a.Revision, &anchor, &a.Intent, &a.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, core.Fail("not_found", "The review attempt is unavailable in this project.", 404)
	}
	if err == nil && json.Unmarshal([]byte(anchor), &a.Anchor) != nil {
		return a, core.Fail("storage_unavailable", "Stored review admission is unavailable.", 500)
	}
	return a, err
}

// Admit persists the exact comparison and external intent before the adapter
// may post or start its command. The caller owns fresh queue/allowance proof.
func (e *Engine) Admit(ctx context.Context, project, id string, expectedRevision int64, anchor Anchor, intent string) (Attempt, error) {
	observed, err := time.Parse(time.RFC3339Nano, anchor.ObservedAt)
	if err != nil || time.Since(observed) > time.Minute || observed.After(time.Now().Add(5*time.Second)) || anchor.RepositoryID <= 0 || anchor.ParentPullNumber < 0 || anchor.ParentPullNumber > 2147483647 || !safeText(anchor.BaseRef, 256) || anchor.BaseRef == "" || !repository.MatchString(anchor.Repository) || !sha.MatchString(anchor.CandidateSHA) || !sha.MatchString(anchor.BaseSHA) || !sha.MatchString(anchor.ParentSHA) || !sha.MatchString(anchor.MergeBaseSHA) || !hash.MatchString(anchor.PatchSHA256) || !safeText(intent, 4096) {
		return Attempt{}, invalid()
	}
	var result Attempt
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if a.Revision != expectedRevision || a.State != "preparing" {
			return conflict()
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		if anchor.PullNumber != o.PullNumber || anchor.CandidateSHA != o.HeadSHA {
			return conflict()
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != anchor.RepositoryID {
			return conflict()
		}
		c, revision, err := readConfiguration(ctx, tx, project)
		if err != nil {
			return err
		}
		allowed, err := actorAllowed(ctx, tx, project, o.ActorID, o.WorkerID)
		if err != nil {
			return err
		}
		if !allowed || c.Paused || revision != o.ConfigurationRevision {
			return conflict()
		}
		capability, ok := e.capabilities[c.CapabilityID]
		if !ok || e.validateConfiguration(c) != nil || capability.Provider != a.Provider || capability.Account != a.Account || capability.Fingerprint != o.CapabilityFingerprint {
			return conflict()
		}
		state := "active"
		if o.Channel == "hosted" {
			state = "posting"
			if intent == "" {
				return invalid()
			}
		} else if intent != "" {
			return invalid()
		}
		a.State = state
		a.Revision++
		a.Anchor = &anchor
		a.Intent = intent
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=?,anchor_json=?,intent=? WHERE id=?`, a.State, a.Revision, encoded(anchor), intent, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,updated_at=? WHERE id=?`, state, stamp(), o.ID); err != nil {
			return err
		}
		if err = transitionRow(ctx, tx, a, "admitted"); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}

func safeText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func transitionRow(ctx context.Context, tx *sql.Tx, a Attempt, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at) VALUES(?,?,?,?,?,?)`, a.ID, a.Revision, a.State, reason, a.ExternalID, stamp())
	return err
}

// Advance records one guarded transition. Uncertain attempts retain both
// reservations; no retry, lease expiry or restart silently spends another run.
func (e *Engine) Advance(ctx context.Context, project, id string, expectedRevision int64, state, reason, externalID string) (Attempt, error) {
	if !identifier.MatchString(reason) || externalID != "" && !identifier.MatchString(externalID) {
		return Attempt{}, invalid()
	}
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if a.Revision != expectedRevision || !allowedTransition(a.State, state) {
			return conflict()
		}
		if a.ExternalID != "" && externalID != "" && externalID != a.ExternalID {
			return conflict()
		}
		if externalID != "" {
			a.ExternalID = externalID
		}
		if a.State == "posting" && state == "active" && a.ExternalID == "" {
			return invalid()
		}
		a.State = state
		a.Revision++
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=?,external_id=? WHERE id=?`, state, a.Revision, a.ExternalID, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,reason=?,updated_at=? WHERE id=?`, state, reason, stamp(), a.OperationID); err != nil {
			return err
		}
		if state != "posting" && state != "active" && state != "uncertain" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
				return err
			}
		}
		if err = transitionRow(ctx, tx, a, reason); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}

func allowedTransition(from, to string) bool {
	switch from {
	case "preparing":
		return to == "held" || to == "failed" || to == "cancelled" || to == "rate_limited"
	case "posting":
		return to == "active" || to == "uncertain" || to == "failed" || to == "rate_limited"
	case "active":
		return to == "completed" || to == "failed" || to == "cancelled" || to == "rate_limited" || to == "uncertain"
	case "uncertain":
		return to == "active" || to == "completed" || to == "failed" || to == "cancelled" || to == "rate_limited"
	}
	return false
}

// RecoverInterrupted runs once before a new supervisor accepts work. It never
// restarts a possibly dispatched action. Hosted recovery uses its retained
// marker; interrupted CLI work remains uncertain until evidence settles it.
func (e *Engine) RecoverInterrupted(ctx context.Context) error {
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_execution_attempts WHERE state IN ('preparing','posting','active') ORDER BY id`)
		if err != nil {
			return err
		}
		type identity struct{ project, id string }
		var ids []identity
		for rows.Next() {
			var x identity
			if err = rows.Scan(&x.project, &x.id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, x)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, x := range ids {
			a, err := readAttempt(ctx, tx, x.project, x.id)
			if err != nil {
				return err
			}
			state := "uncertain"
			if a.State == "preparing" {
				state = "cancelled"
			}
			a.State = state
			a.Revision++
			if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=? WHERE id=?`, state, a.Revision, a.ID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,reason='supervisor_interrupted',revision=revision+1,updated_at=? WHERE id=?`, state, stamp(), a.OperationID); err != nil {
				return err
			}
			if state == "cancelled" {
				if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
					return err
				}
			}
			if err = transitionRow(ctx, tx, a, "supervisor_interrupted"); err != nil {
				return err
			}
		}
		return nil
	})
}
