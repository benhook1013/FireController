package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// CLIAllowanceObservation records successfully observed included allowance and
// its source comparison. It neither grants execution permission nor spends it.
type CLIAllowanceObservation struct {
	Anchor           Anchor `json:"anchor"`
	ObservedAt       string `json:"observed_at"`
	Remaining        int64  `json:"remaining"`
	Limit            int64  `json:"limit"`
	FullCapacityInMS int64  `json:"full_capacity_in_ms"`
	RollingWindowMS  int64  `json:"rolling_window_ms"`
	ChangedFiles     int64  `json:"changed_files"`
}

type cliAllowanceRecord struct {
	ProjectID             string                  `json:"project_id"`
	AttemptID             string                  `json:"attempt_id"`
	AttemptRevision       int64                   `json:"attempt_revision"`
	OperationID           string                  `json:"operation_id"`
	ActorID               string                  `json:"actor_id"`
	WorkerID              string                  `json:"worker_id"`
	ConfigurationRevision int64                   `json:"configuration_revision"`
	PolicyRevision        int64                   `json:"policy_revision"`
	CapabilityFingerprint string                  `json:"capability_fingerprint"`
	Provider              string                  `json:"provider"`
	Account               string                  `json:"account"`
	Observation           CLIAllowanceObservation `json:"observation"`
}

func validCLIAllowance(input CLIAllowanceObservation) bool {
	return validCLIAllowanceObservation(input) && input.Remaining > 0
}

func validCLIAllowanceObservation(input CLIAllowanceObservation) bool {
	if len(input.ObservedAt) > 64 || len(input.Anchor.ObservedAt) > 64 {
		return false
	}
	observed, err := time.Parse(time.RFC3339Nano, input.ObservedAt)
	return err == nil && !observed.IsZero() && observed.Year() >= 1 && observed.Year() <= 9999 && input.ObservedAt == observed.UTC().Format(time.RFC3339Nano) && validEvidenceAnchor(input.Anchor) &&
		input.Remaining >= 0 && input.Limit >= 1 && input.Limit <= 10000 && input.Remaining <= input.Limit && input.FullCapacityInMS >= 0 && input.FullCapacityInMS <= 86400000 && input.RollingWindowMS >= 1 && input.RollingWindowMS <= 86400000 && input.ChangedFiles >= 1 && input.ChangedFiles <= 100000
}

func freshAllowanceObservation(value string, now time.Time) bool {
	observed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !observed.Before(now.Add(-time.Minute)) && !observed.After(now.Add(5*time.Second))
}

// RecordCLIAllowance retains one immutable preparing-attempt observation inside
// a local transaction. Exact replay precedes current admission and freshness
// checks; it never authorizes another provider action or refreshes old facts.
func (e *Engine) RecordCLIAllowance(ctx context.Context, project, attemptID string, expectedRevision int64, input CLIAllowanceObservation) error {
	if !validCLIAllowance(input) {
		return invalid()
	}
	return e.recordCLIAllowance(ctx, project, attemptID, expectedRevision, input)
}

// RecordCLIExhaustion retains observed zero capacity without a review permit.
func (e *Engine) RecordCLIExhaustion(ctx context.Context, project, attemptID string, expectedRevision int64, input CLIAllowanceObservation) error {
	if input.Remaining != 0 {
		return invalid()
	}
	return e.recordCLIAllowance(ctx, project, attemptID, expectedRevision, input)
}

func (e *Engine) recordCLIAllowance(ctx context.Context, project, attemptID string, expectedRevision int64, input CLIAllowanceObservation) error {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) || expectedRevision < 1 || !validCLIAllowanceObservation(input) {
		return invalid()
	}
	if expectedRevision != 1 {
		return conflict()
	}
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_cli_preflight WHERE project_id=? AND attempt_id=?`, project, attemptID).Scan(&body)
		if err == nil {
			var retained cliAllowanceRecord
			if len(body) > 8192 || strictjson.Decode([]byte(body), &retained) != nil || retained.ProjectID != project || retained.AttemptID != attemptID || !identifier.MatchString(retained.OperationID) || !identifier.MatchString(retained.ActorID) || retained.WorkerID != "" && !identifier.MatchString(retained.WorkerID) || retained.AttemptRevision != 1 || retained.ConfigurationRevision < 1 || retained.PolicyRevision < 1 || !hash.MatchString(retained.CapabilityFingerprint) || !identifier.MatchString(retained.Provider) || !identifier.MatchString(retained.Account) || !validCLIAllowanceObservation(retained.Observation) {
				return core.Fail("storage_unavailable", "Stored CLI allowance observation is unavailable.", 500)
			}
			if retained.AttemptRevision != expectedRevision || retained.Observation != input {
				return conflict()
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		attempt, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		if attempt.Revision != expectedRevision || attempt.State != "preparing" || attempt.Anchor != nil {
			return conflict()
		}
		operation, err := readOperation(ctx, tx, project, attempt.OperationID)
		if err != nil {
			return err
		}
		if operation.AttemptID != attempt.ID || operation.State != "preparing" || operation.Channel != "cli" || !identifier.MatchString(operation.ActorID) || operation.WorkerID != "" && !identifier.MatchString(operation.WorkerID) || input.Anchor.PullNumber != operation.PullNumber || input.Anchor.CandidateSHA != operation.HeadSHA {
			return conflict()
		}
		checked := operation
		if input.Remaining == 0 {
			// A negative observation grants no permit. Neighbor enrollment or
			// ordering changes must not discard its already observed quota facts.
			// Keep the record bound to the original operation/configuration, and
			// still check current target membership, access, policy and controls.
			configuration, revision, err := readConfiguration(ctx, tx, project)
			if err != nil {
				return err
			}
			original, err := sourceConfiguration(ctx, tx, project, operation.ConfigurationRevision)
			if err != nil {
				return err
			}
			if configuration.CapabilityID != original.CapabilityID || configuration.Mode != original.Mode {
				return conflict()
			}
			checked.ConfigurationRevision = revision
		}
		current, err := e.executionContext(ctx, tx, checked, input.Remaining == 0)
		if err != nil {
			return err
		}
		capability := e.capabilities[current.Configuration.CapabilityID]
		if attempt.Provider != capability.Provider || attempt.Account != capability.Account || input.Anchor.RepositoryID != current.RepositoryID {
			return conflict()
		}
		now := time.Now()
		if !freshAllowanceObservation(input.ObservedAt, now) || !freshAllowanceObservation(input.Anchor.ObservedAt, now) {
			return invalid()
		}
		if ceiling := current.Policy.Policy.MaxReviewFiles; !current.Operation.OverrideFileLimit && ceiling > 0 && input.ChangedFiles > ceiling {
			return sourceHold("project_file_limit")
		}
		record := cliAllowanceRecord{ProjectID: project, AttemptID: attempt.ID, AttemptRevision: expectedRevision, OperationID: operation.ID, ActorID: operation.ActorID, WorkerID: operation.WorkerID, ConfigurationRevision: operation.ConfigurationRevision, PolicyRevision: operation.PolicyRevision, CapabilityFingerprint: operation.CapabilityFingerprint, Provider: attempt.Provider, Account: attempt.Account, Observation: input}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_cli_preflight(attempt_id,project_id,body_json) VALUES(?,?,?)`, attempt.ID, project, encoded(record)); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}, "review.cli_preflight", attempt.ID, 1, struct {
			OperationID           string `json:"operation_id"`
			ConfigurationRevision int64  `json:"configuration_revision"`
			PolicyRevision        int64  `json:"policy_revision"`
			RepositoryID          int64  `json:"repository_id"`
			PullNumber            int64  `json:"pull_number"`
			HeadSHA               string `json:"head_sha"`
			Remaining             int64  `json:"remaining"`
			Limit                 int64  `json:"limit"`
			ChangedFiles          int64  `json:"changed_files"`
			ObservedAt            string `json:"observed_at"`
		}{operation.ID, operation.ConfigurationRevision, operation.PolicyRevision, current.RepositoryID, operation.PullNumber, operation.HeadSHA, input.Remaining, input.Limit, input.ChangedFiles, input.ObservedAt})
	})
}

// CLIExhaustionRecovery is private, trusted negative evidence, never a permit.
// It is not accepted by HTTP or MCP and retains the original observation time.
type CLIExhaustionRecovery struct {
	Operation   Operation               `json:"operation"`
	Attempt     Attempt                 `json:"attempt"`
	Observation CLIAllowanceObservation `json:"observation"`
}
type CLIExhaustionRecoverySource interface {
	LoadCLIExhaustion(context.Context, string) (CLIExhaustionRecovery, bool, error)
}

func WithCLIExhaustionRecovery(source CLIExhaustionRecoverySource) EngineOption {
	return func(e *Engine) { e.cliExhaustionRecovery = source }
}

// ReplayCLIExhaustion restores only an unused, exactly bound zero observation.
// Current authority is deliberately not consulted: the subsequent retry still
// requires normal admission. The journal write and cooldown are one transaction.
func (e *Engine) ReplayCLIExhaustion(ctx context.Context, r CLIExhaustionRecovery) error {
	if err := ValidateCLIExhaustionRecovery(r); err != nil {
		return err
	}
	in := r.Observation
	original := r.Operation
	captured := r.Attempt
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, captured.ProjectID, captured.ID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, captured.ProjectID, captured.OperationID)
		if err != nil {
			return err
		}
		if a.OperationID != o.ID || a.ProjectID != o.ProjectID || o.ID != original.ID || o.ProjectID != original.ProjectID {
			return invalid()
		}
		if o.ActorID != original.ActorID || o.WorkerID != original.WorkerID || o.ConfigurationRevision != original.ConfigurationRevision || o.PolicyRevision != original.PolicyRevision || o.CapabilityFingerprint != original.CapabilityFingerprint || o.PullNumber != original.PullNumber || o.HeadSHA != original.HeadSHA || o.Channel != "cli" || a.Provider != captured.Provider || a.Account != captured.Account || in.Anchor.PullNumber != o.PullNumber || in.Anchor.CandidateSHA != o.HeadSHA {
			return invalid()
		}
		var repository int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, o.ProjectID).Scan(&repository); err != nil {
			return err
		}
		if repository != in.Anchor.RepositoryID {
			return invalid()
		}
		record := cliAllowanceRecord{ProjectID: o.ProjectID, AttemptID: a.ID, AttemptRevision: 1, OperationID: o.ID, ActorID: o.ActorID, WorkerID: o.WorkerID, ConfigurationRevision: o.ConfigurationRevision, PolicyRevision: o.PolicyRevision, CapabilityFingerprint: o.CapabilityFingerprint, Provider: a.Provider, Account: a.Account, Observation: in}
		var prior string
		err = tx.QueryRowContext(ctx, `SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		exists := err == nil
		if exists && prior != encoded(record) {
			return invalid()
		}
		if exists && a.State == "held" && a.Revision == 2 && o.State == "queued" && o.AttemptID == "" && o.Wait != nil {
			baseline := captured.ID
			if original.Wait != nil && original.Wait.BaselineAttemptID != "" {
				baseline = original.Wait.BaselineAttemptID
			}
			var reservations int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&reservations); err != nil {
				return err
			}
			if o.Wait.BaselineAttemptID != baseline || o.Revision != original.Revision+1 || reservations != 0 {
				return invalid()
			}
			return nil
		}
		if a.State != "preparing" || a.Revision != 1 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || o.State != "preparing" || o.AttemptID != a.ID || o.Revision != original.Revision {
			return invalid()
		}
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND repository_id=? AND pull_number=? AND action_class='cli'`, a.ID, a.Provider, a.Account, repository, o.PullNumber).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 1 {
			return invalid()
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_cli_preflight(attempt_id,project_id,body_json) VALUES(?,?,?)`, a.ID, o.ProjectID, encoded(record)); err != nil {
				return err
			}
			if err := core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.cli_preflight", a.ID, 1, struct {
				OperationID           string `json:"operation_id"`
				ConfigurationRevision int64  `json:"configuration_revision"`
				PolicyRevision        int64  `json:"policy_revision"`
				RepositoryID          int64  `json:"repository_id"`
				PullNumber            int64  `json:"pull_number"`
				HeadSHA               string `json:"head_sha"`
				Remaining             int64  `json:"remaining"`
				Limit                 int64  `json:"limit"`
				ChangedFiles          int64  `json:"changed_files"`
				ObservedAt            string `json:"observed_at"`
			}{o.ID, o.ConfigurationRevision, o.PolicyRevision, r.Observation.Anchor.RepositoryID, o.PullNumber, o.HeadSHA, r.Observation.Remaining, r.Observation.Limit, r.Observation.ChangedFiles, r.Observation.ObservedAt}); err != nil {
				return err
			}
		}
		_, err = e.scheduleCLICooldown(ctx, tx, o.ProjectID, a.ID, a.Revision)
		return err
	})
}

func (e *Engine) reconcileCLIExhaustions(ctx context.Context, skipped map[string]bool) error {
	if e.cliExhaustionRecovery == nil {
		return nil
	}
	rows, err := e.store.DB().QueryContext(ctx, `SELECT a.id FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id AND o.attempt_id=a.id WHERE a.state='preparing' AND o.state='preparing' AND o.channel='cli' AND NOT EXISTS(SELECT 1 FROM review_cli_preflight p WHERE p.attempt_id=a.id) ORDER BY a.id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		local, cancel := context.WithTimeout(ctx, 3*time.Second)
		record, found, err := e.cliExhaustionRecovery.LoadCLIExhaustion(local, id)
		if err == nil && found {
			err = e.ReplayCLIExhaustion(local, record)
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			var refusal *core.Error
			if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
				continue
			}
			skipped[id] = true
			slog.WarnContext(ctx, "CLI exhaustion recovery unavailable", "attempt_id", id, "reason", "cli_exhaustion_recovery_unavailable")
		}
	}
	return nil
}

func ValidateCLIExhaustionRecovery(r CLIExhaustionRecovery) error {
	in := r.Observation
	original := r.Operation
	captured := r.Attempt
	if in.Remaining != 0 || !validCLIAllowanceObservation(in) || captured.Revision != 1 || captured.State != "preparing" || captured.Anchor != nil || captured.Intent != "" || captured.ExternalID != "" || original.State != "preparing" || original.Channel != "cli" || original.AttemptID != captured.ID || original.ID != captured.OperationID || original.ProjectID != captured.ProjectID {
		return invalid()
	}

	if !hostedObservationID(captured.ID, "review_attempt_") || !hostedObservationID(original.ID, "review_operation_") || !identifier.MatchString(original.ProjectID) || !identifier.MatchString(original.ActorID) || original.WorkerID != "" && !identifier.MatchString(original.WorkerID) || !identifier.MatchString(captured.Provider) || !identifier.MatchString(captured.Account) || !hash.MatchString(original.CapabilityFingerprint) || original.ConfigurationRevision < 1 || original.PolicyRevision < 1 || original.Revision < 2 {
		return invalid()
	}
	return nil
}
