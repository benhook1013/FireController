package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// HostedAllowanceInput binds a reference-checked private query capture and its
// fresh attributed availability to the exact source. It is trusted runtime
// input, never a client upload or a grant of permission to post a review.
type HostedAllowanceInput struct {
	QueryID       string                                    `json:"query_id"`
	Anchor        Anchor                                    `json:"anchor"`
	ObservedAt    string                                    `json:"observed_at"`
	ChangedFiles  int64                                     `json:"changed_files"`
	CaptureID     string                                    `json:"capture_id"`
	CaptureSHA256 string                                    `json:"capture_sha256"`
	CaptureBytes  int64                                     `json:"capture_bytes"`
	Observation   reviewprovider.HostedAllowanceObservation `json:"observation"`
}

type hostedAllowanceRecord struct {
	ProjectID             string               `json:"project_id"`
	AttemptID             string               `json:"attempt_id"`
	OperationID           string               `json:"operation_id"`
	ActorID               string               `json:"actor_id"`
	WorkerID              string               `json:"worker_id"`
	ConfigurationRevision int64                `json:"configuration_revision"`
	PolicyRevision        int64                `json:"policy_revision"`
	CapabilityFingerprint string               `json:"capability_fingerprint"`
	Input                 HostedAllowanceInput `json:"input"`
}

func hostedAllowanceSameSource(a, b Anchor) bool {
	a.Repository, a.ObservedAt = b.Repository, b.ObservedAt
	return a == b
}

func validHostedAllowanceInput(in HostedAllowanceInput) bool {
	observed, ok := hostedObservationTime(in.ObservedAt)
	o := in.Observation
	return ok && hostedObservationID(in.QueryID, "review_hosted_query_") && validEvidenceAnchor(in.Anchor) &&
		in.ChangedFiles > 0 && in.ChangedFiles <= 100000 && hash.MatchString(in.CaptureSHA256) && in.CaptureID == "review_capture_"+in.CaptureSHA256[:32] && in.CaptureBytes > 0 && in.CaptureBytes <= 48<<20 &&
		o.State == "available_observed" && o.Reason == "provider_available_observed" && o.QueryCommentID > 0 && o.ResponseCommentID > 0 && o.ResponseCommentID != o.QueryCommentID &&
		o.QueryCreatedAt.Location() == time.UTC && !o.QueryCreatedAt.IsZero() && o.QueryCreatedAt.Year() >= 1 && o.QueryCreatedAt.Year() <= 9999 &&
		o.ResponseCreatedAt.Location() == time.UTC && !o.ResponseCreatedAt.IsZero() && o.ResponseCreatedAt.After(o.QueryCreatedAt) && !o.ResponseCreatedAt.After(observed) && o.RetryNotBefore.IsZero()
}

func hostedAllowanceUnavailable() error {
	return core.Fail("storage_unavailable", "Stored hosted allowance observation is unavailable.", 500)
}

func decodeHostedAllowance(body, project, attemptID string) (hostedAllowanceRecord, error) {
	var retained hostedAllowanceRecord
	if len(body) > 16<<10 || strictjson.Validate([]byte(body)) != nil || json.Unmarshal([]byte(body), &retained) != nil || encoded(retained) != body ||
		retained.ProjectID != project || retained.AttemptID != attemptID || !hostedObservationID(retained.OperationID, "review_operation_") || !identifier.MatchString(retained.ActorID) || retained.WorkerID != "" && !identifier.MatchString(retained.WorkerID) ||
		retained.ConfigurationRevision < 1 || retained.PolicyRevision < 1 || !hash.MatchString(retained.CapabilityFingerprint) || !validHostedAllowanceInput(retained.Input) {
		return hostedAllowanceRecord{}, hostedAllowanceUnavailable()
	}
	return retained, nil
}

func hostedAllowanceBinding(record hostedAllowanceRecord, attempt Attempt, operation Operation, query HostedAllowanceQuery) bool {
	return record.ProjectID == attempt.ProjectID && record.ProjectID == operation.ProjectID && record.AttemptID == attempt.ID && operation.AttemptID == attempt.ID && attempt.OperationID == operation.ID &&
		record.OperationID == operation.ID && record.ActorID == operation.ActorID && record.WorkerID == operation.WorkerID && record.ConfigurationRevision == operation.ConfigurationRevision && record.PolicyRevision == operation.PolicyRevision &&
		record.CapabilityFingerprint == operation.CapabilityFingerprint && operation.Channel == "hosted" && record.Input.QueryID == query.ID && query.AttemptID == attempt.ID && query.OperationID == operation.ID &&
		query.Comment != nil && query.Comment.ID == record.Input.Observation.QueryCommentID && query.Comment.CreatedAt.Equal(record.Input.Observation.QueryCreatedAt) && hostedAllowanceSameSource(record.Input.Anchor, query.Anchor)
}

// RecordHostedAllowance retains one immutable positive observation before review
// admission. Exact replay is local and precedes freshness/current eligibility;
// a replay cannot refresh provider facts or authorize another external action.
func (e *Engine) RecordHostedAllowance(ctx context.Context, project, attemptID string, expectedRevision int64, input HostedAllowanceInput) error {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || expectedRevision != 1 || !validHostedAllowanceInput(input) {
		return invalid()
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(canonical) > 12<<10 {
		return invalid()
	}
	// Own the input before crossing the writer boundary.
	var owned HostedAllowanceInput
	if json.Unmarshal(canonical, &owned) != nil {
		return invalid()
	}
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_hosted_preflight WHERE project_id=? AND attempt_id=?`, project, attemptID).Scan(&body)
		if err == nil {
			retained, err := decodeHostedAllowance(body, project, attemptID)
			if err != nil {
				return err
			}
			if encoded(retained.Input) != string(canonical) {
				return conflict()
			}
			attempt, err := readAttempt(ctx, tx, project, attemptID)
			if err != nil {
				return err
			}
			operation, err := readOperation(ctx, tx, project, attempt.OperationID)
			if err != nil {
				return err
			}
			query, err := readHostedQuery(ctx, tx, attempt, operation)
			if err != nil {
				return err
			}
			if !hostedAllowanceBinding(retained, attempt, operation, query) {
				return hostedAllowanceUnavailable()
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
		operation, err := readOperation(ctx, tx, project, attempt.OperationID)
		if err != nil {
			return err
		}
		if attempt.Revision != expectedRevision || attempt.State != "preparing" || attempt.Anchor != nil || operation.State != "preparing" || operation.AttemptID != attempt.ID || operation.Channel != "hosted" || operation.PullNumber != owned.Anchor.PullNumber || operation.HeadSHA != owned.Anchor.CandidateSHA {
			return conflict()
		}
		query, err := readHostedQuery(ctx, tx, attempt, operation)
		if err != nil {
			return err
		}
		observation := owned.Observation
		if query.ID != owned.QueryID || query.Comment == nil || query.Comment.ID != observation.QueryCommentID || !query.Comment.CreatedAt.Equal(observation.QueryCreatedAt) || !hostedAllowanceSameSource(query.Anchor, owned.Anchor) {
			return conflict()
		}
		current, err := e.currentExecutionContext(ctx, tx, operation)
		if err != nil {
			return err
		}
		capability := e.capabilities[current.Configuration.CapabilityID]
		if attempt.Provider != capability.Provider || attempt.Account != capability.Account || query.CapabilityFingerprint != operation.CapabilityFingerprint || owned.Anchor.RepositoryID != current.RepositoryID {
			return conflict()
		}
		now := time.Now().UTC()
		if !freshAllowanceObservation(owned.ObservedAt, now) || !freshAllowanceObservation(owned.Anchor.ObservedAt, now) || observation.ResponseCreatedAt.Before(now.Add(-time.Minute)) || observation.ResponseCreatedAt.After(now.Add(5*time.Second)) {
			return invalid()
		}
		if ceiling := current.Policy.Policy.MaxReviewFiles; ceiling > 0 && owned.ChangedFiles > ceiling {
			return sourceHold("project_file_limit")
		}
		record := hostedAllowanceRecord{ProjectID: project, AttemptID: attempt.ID, OperationID: operation.ID, ActorID: operation.ActorID, WorkerID: operation.WorkerID,
			ConfigurationRevision: operation.ConfigurationRevision, PolicyRevision: operation.PolicyRevision, CapabilityFingerprint: operation.CapabilityFingerprint, Input: owned}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_hosted_preflight(attempt_id,project_id,body_json) VALUES(?,?,?)`, attempt.ID, project, encoded(record)); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}, "review.hosted_preflight", attempt.ID, 1, struct {
			OperationID string `json:"operation_id"`
			QueryID     string `json:"query_id"`
			CaptureID   string `json:"capture_id"`
		}{operation.ID, query.ID, owned.CaptureID})
	})
}

func readHostedAllowance(ctx context.Context, tx *sql.Tx, project, attemptID string) (hostedAllowanceRecord, Attempt, Operation, error) {
	var body string
	err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_hosted_preflight WHERE project_id=? AND attempt_id=?`, project, attemptID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return hostedAllowanceRecord{}, Attempt{}, Operation{}, core.Fail("not_found", "The hosted allowance observation is unavailable in this project.", 404)
	}
	if err != nil {
		return hostedAllowanceRecord{}, Attempt{}, Operation{}, err
	}
	record, err := decodeHostedAllowance(body, project, attemptID)
	if err != nil {
		return record, Attempt{}, Operation{}, err
	}
	attempt, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return record, attempt, Operation{}, err
	}
	operation, err := readOperation(ctx, tx, project, attempt.OperationID)
	if err != nil {
		return record, attempt, operation, err
	}
	query, err := readHostedQuery(ctx, tx, attempt, operation)
	if err != nil {
		return record, attempt, operation, err
	}
	if !hostedAllowanceBinding(record, attempt, operation, query) {
		return record, attempt, operation, hostedAllowanceUnavailable()
	}
	return record, attempt, operation, nil
}

// ReadHostedAllowance returns original retained facts for trusted recovery. It
// neither refreshes their timestamps nor asserts that they remain applicable.
func (e *Engine) ReadHostedAllowance(ctx context.Context, project, attemptID string) (HostedAllowanceInput, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") {
		return HostedAllowanceInput{}, invalid()
	}
	var input HostedAllowanceInput
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		record, _, _, err := readHostedAllowance(ctx, tx, project, attemptID)
		input = record.Input
		return err
	})
	if err != nil {
		return HostedAllowanceInput{}, err
	}
	return input, nil
}

// VerifyHostedAction combines the common final action guard and the retained
// query/availability proof in one SQL snapshot. Fresh provider source/activity
// checks remain the driver's responsibility; this method performs no I/O there.
func (e *Engine) VerifyHostedAction(ctx context.Context, project, attemptID string, expectedRevision int64, predecessors ...SourcePlan) error {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || expectedRevision < 1 {
		return invalid()
	}
	return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := e.verifyAction(ctx, tx, project, attemptID, expectedRevision, predecessors); err != nil {
			return err
		}
		record, attempt, operation, err := readHostedAllowance(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		if operation.Channel != "hosted" || attempt.State != "posting" || attempt.Anchor == nil || !hostedAllowanceSameSource(record.Input.Anchor, *attempt.Anchor) {
			return conflict()
		}
		intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: operation.ID, AttemptID: attempt.ID, HeadSHA: operation.HeadSHA})
		if err != nil || attempt.Intent != intent || attempt.ExternalID != "" {
			return conflict()
		}
		current, err := e.currentExecutionContext(ctx, tx, operation)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !freshAllowanceObservation(record.Input.ObservedAt, now) || !freshAllowanceObservation(record.Input.Anchor.ObservedAt, now) || !freshAllowanceObservation(attempt.Anchor.ObservedAt, now) || record.Input.Observation.ResponseCreatedAt.Before(now.Add(-time.Minute)) || record.Input.Observation.ResponseCreatedAt.After(now.Add(5*time.Second)) {
			return sourceHold("allowance_stale")
		}
		if ceiling := current.Policy.Policy.MaxReviewFiles; ceiling > 0 && record.Input.ChangedFiles > ceiling {
			return sourceHold("project_file_limit")
		}
		return nil
	})
}
