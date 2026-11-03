package reviewexec

import (
	"context"
	"database/sql"
	"errors"
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
		current, err := e.currentExecutionContext(ctx, tx, checked)
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
