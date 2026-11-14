package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// IntentLineage keeps PR-level intent separate from immutable execution generations.
// It conveys no positive allowance, admission, or permission to repeat an external action.
type IntentLineage struct {
	RootOperationID string            `json:"root_operation_id"`
	PredecessorID   string            `json:"predecessor_operation_id"`
	SuccessorID     string            `json:"successor_operation_id"`
	IntentCreatedAt string            `json:"intent_created_at"`
	NotBefore       string            `json:"not_before,omitempty"`
	CLIExhaustions  int               `json:"cli_exhaustions"`
	SourceRetries   int               `json:"source_retries"`
	Depth           int               `json:"depth"`
	ReboundAt       string            `json:"rebound_at"`
	Source          SourceObservation `json:"source"`
}

func IntentRebindingSchemaStatements() []string {
	return []string{
		`ALTER TABLE review_operations ADD COLUMN intent_predecessor_id TEXT REFERENCES review_operations(id)`,
		`CREATE TABLE review_intent_rebindings(successor_operation_id TEXT PRIMARY KEY REFERENCES review_operations(id),predecessor_operation_id TEXT NOT NULL UNIQUE REFERENCES review_operations(id),root_operation_id TEXT NOT NULL REFERENCES review_operations(id),project_id TEXT NOT NULL REFERENCES projects(id),intent_created_at TEXT NOT NULL,not_before TEXT NOT NULL,cli_exhaustions INTEGER NOT NULL CHECK(cli_exhaustions BETWEEN 0 AND 3),source_retries INTEGER NOT NULL CHECK(source_retries BETWEEN 0 AND 3),depth INTEGER NOT NULL CHECK(depth BETWEEN 1 AND 1000),rebound_at TEXT NOT NULL,source_json TEXT NOT NULL CHECK(length(CAST(source_json AS BLOB))<=4096),CHECK(successor_operation_id<>predecessor_operation_id))`,
		`CREATE INDEX review_intent_root ON review_intent_rebindings(root_operation_id,depth)`,
	}
}

func ensureIntentRebindingSchema(ctx context.Context, s *core.Store) error {
	return s.EnsureSchema(ctx, "review_intent_rebindings", IntentRebindingSchemaStatements())
}

func readIntentLineage(ctx context.Context, q EvidenceQuery, project, successor string) (*IntentLineage, error) {
	var domain string
	err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_intent_rebindings'`).Scan(&domain)
	if errors.Is(err, sql.ErrNoRows) {
		var table bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_intent_rebindings')`).Scan(&table); err != nil {
			return nil, err
		}
		var markerColumn bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('review_operations') WHERE name='intent_predecessor_id')`).Scan(&markerColumn); err != nil {
			return nil, err
		}
		if table || markerColumn {
			return nil, waitUnavailable()
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if domain != "1" {
		return nil, waitUnavailable()
	}
	var marker sql.NullString
	if err = q.QueryRowContext(ctx, `SELECT intent_predecessor_id FROM review_operations WHERE project_id=? AND id=?`, project, successor).Scan(&marker); err != nil {
		return nil, err
	}
	marked := marker.Valid
	var v IntentLineage
	var body string
	err = q.QueryRowContext(ctx, `SELECT root_operation_id,predecessor_operation_id,successor_operation_id,intent_created_at,not_before,cli_exhaustions,source_retries,depth,rebound_at,source_json FROM review_intent_rebindings WHERE project_id=? AND successor_operation_id=?`, project, successor).Scan(&v.RootOperationID, &v.PredecessorID, &v.SuccessorID, &v.IntentCreatedAt, &v.NotBefore, &v.CLIExhaustions, &v.SourceRetries, &v.Depth, &v.ReboundAt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		if marked {
			return nil, waitUnavailable()
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !marked || marker.String != v.PredecessorID {
		return nil, waitUnavailable()
	}
	if json.Unmarshal([]byte(body), &v.Source) != nil || !validSourceObservation(v.Source) || v.Depth < 1 || v.Depth > 1000 || v.CLIExhaustions < 0 || v.CLIExhaustions > 3 || v.SourceRetries < 0 || v.SourceRetries > sourceRetryLimit {
		return nil, waitUnavailable()
	}
	created, ok := attemptRecordedTime(v.IntentCreatedAt)
	at, atOK := attemptRecordedTime(v.ReboundAt)
	observed, observedOK := attemptRecordedTime(v.Source.ObservedAt)
	if !ok || !atOK || at.Before(created) || !observedOK || observed.After(at) || at.Sub(observed) > time.Minute {
		return nil, waitUnavailable()
	}
	if v.NotBefore != "" {
		if _, ok := attemptRecordedTime(v.NotBefore); !ok {
			return nil, waitUnavailable()
		}
	}
	var oldProject, newProject, rootProject, oldHead, newHead, rootCreated, oldState, oldActor, newActor, oldWorker, newWorker, oldChannel, newChannel string
	var oldPull, newPull int64
	err = q.QueryRowContext(ctx, `SELECT o.project_id,n.project_id,r.project_id,o.head_sha,n.head_sha,r.created_at,o.state,o.actor_id,n.actor_id,o.worker_id,n.worker_id,o.channel,n.channel,o.pull_number,n.pull_number FROM review_operations o JOIN review_operations n ON n.id=? JOIN review_operations r ON r.id=? WHERE o.id=?`, v.SuccessorID, v.RootOperationID, v.PredecessorID).Scan(&oldProject, &newProject, &rootProject, &oldHead, &newHead, &rootCreated, &oldState, &oldActor, &newActor, &oldWorker, &newWorker, &oldChannel, &newChannel, &oldPull, &newPull)
	if err != nil {
		return nil, err
	}
	if oldProject != project || newProject != project || rootProject != project || rootCreated != v.IntentCreatedAt || oldState != "cancelled" || oldActor != newActor || oldWorker != newWorker || oldChannel != newChannel || oldPull != newPull || v.Source.PullNumber != newPull || v.Source.HeadSHA != newHead || !sha.MatchString(oldHead) {
		return nil, waitUnavailable()
	}
	var parentRoot string
	var parentDepth, parentCLI, parentRetries int
	var parentFloor string
	err = q.QueryRowContext(ctx, `SELECT root_operation_id,depth,cli_exhaustions,source_retries,not_before FROM review_intent_rebindings WHERE successor_operation_id=? AND project_id=?`, v.PredecessorID, project).Scan(&parentRoot, &parentDepth, &parentCLI, &parentRetries, &parentFloor)
	if errors.Is(err, sql.ErrNoRows) {
		if v.RootOperationID != v.PredecessorID || v.Depth != 1 {
			return nil, waitUnavailable()
		}
	} else if err != nil {
		return nil, err
	} else if parentRoot != v.RootOperationID || v.Depth != parentDepth+1 {
		return nil, waitUnavailable()
	}
	// Derive inherited negative budgets from retained predecessor facts. A copied
	// lineage value cannot lower a retry floor or reset provider probe backoff.
	var exhausted, retries int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_execution_attempts a JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision WHERE a.operation_id=? AND t.reason='allowance_exhausted' LIMIT 3)`, v.PredecessorID).Scan(&exhausted); err != nil {
		return nil, err
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM review_source_retries WHERE operation_id=?`, v.PredecessorID).Scan(&retries); err != nil {
		return nil, err
	}
	if v.CLIExhaustions != min(3, parentCLI+exhausted) || v.SourceRetries != parentRetries+retries {
		return nil, waitUnavailable()
	}
	var retainedFloor, sourceFloor string
	if err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT retry_not_before FROM review_operation_waits WHERE operation_id=?),'')`, v.PredecessorID).Scan(&retainedFloor); err != nil {
		return nil, err
	}
	if err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT retry_not_before FROM review_source_retries WHERE operation_id=? ORDER BY sequence DESC LIMIT 1),'')`, v.PredecessorID).Scan(&sourceFloor); err != nil {
		return nil, err
	}
	for _, floor := range []string{parentFloor, retainedFloor, sourceFloor} {
		if floor == "" {
			continue
		}
		due, valid := attemptRecordedTime(floor)
		actual, actualValid := attemptRecordedTime(v.NotBefore)
		if !valid || !actualValid || actual.Before(due) {
			return nil, waitUnavailable()
		}
	}
	return &v, nil
}

// ValidateIntentRebindingSnapshot checks durable lineage rather than trusting a copied deadline.
func ValidateIntentRebindingSnapshot(ctx context.Context, q EvidenceQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT project_id,successor_operation_id FROM review_intent_rebindings UNION SELECT project_id,id FROM review_operations WHERE intent_predecessor_id IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project, id string
		if err = rows.Scan(&project, &id); err != nil {
			return err
		}
		if _, err = readIntentLineage(ctx, q, project, id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func intentWaitElapsed(ctx context.Context, q EvidenceQuery, o Operation, now time.Time) (bool, error) {
	lineage, err := readIntentLineage(ctx, q, o.ProjectID, o.ID)
	if err != nil {
		return false, err
	}
	if lineage == nil || lineage.NotBefore == "" {
		return true, nil
	}
	due, ok := attemptRecordedTime(lineage.NotBefore)
	if !ok {
		return false, waitUnavailable()
	}
	return !now.Before(due), nil
}

// rebindQueuedIntent atomically retires one proven-unused generation and creates
// its replacement. Its caller supplies fresh trusted source reads, never a user
// supplied SHA update. Failure rolls back retirement, accounting and successor.
func (e *Engine) rebindQueuedIntent(ctx context.Context, expected Operation, source SourceObservation) (Operation, error) {
	var result Operation
	if !validSourceObservation(source) || source.PullNumber != expected.PullNumber {
		return result, invalid()
	}
	observed, ok := attemptRecordedTime(source.ObservedAt)
	now := e.sampleExecutionTime()
	if !ok || observed.After(now) || now.Sub(observed) > time.Minute {
		return result, sourceHold("observation_incomplete")
	}
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		result, err = e.rebindQueuedIntentTx(ctx, tx, expected, source)
		return err
	})
	if err == nil {
		e.notifyOperation(expected.ID)
		e.notifyOperation(result.ID)
	}
	return result, err
}

func (e *Engine) rebindQueuedIntentTx(ctx context.Context, tx *sql.Tx, expected Operation, source SourceObservation) (Operation, error) {
	var result Operation
	err := func() error {
		o, err := readOperation(ctx, tx, expected.ProjectID, expected.ID)
		if err != nil {
			return err
		}
		if o.Revision != expected.Revision || o.State != "queued" || o.AttemptID != "" || o.HeadSHA != expected.HeadSHA {
			return conflict()
		}
		if o.Wait != nil && o.Wait.Paused {
			return conflict()
		}
		execution, err := e.currentExecutionContext(ctx, tx, o)
		if err != nil {
			return err
		}
		if source.RepositoryID != execution.RepositoryID {
			return sourceHold("repository_changed")
		}
		selection, err := automaticOperationSelection(ctx, tx, o)
		if err != nil {
			return err
		}
		if selection != "" {
			continuation, err := continuationForSelection(ctx, tx, o.ProjectID, selection)
			if err != nil {
				return err
			}
			if continuation != nil {
				return sourceHold("hosted_continuation_changed")
			}
		}

		// A queued projection is not sufficient proof that prior external work is absent.
		var unsafe bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_attempts a LEFT JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision WHERE a.operation_id=? AND (a.state<>'held' OR a.anchor_json<>'null' OR a.intent<>'' OR a.external_id<>'' OR t.state IS NULL OR t.state<>'held' OR t.reason NOT IN ('allowance_exhausted','hosted_allowance_cooldown','source_rate_limit_wait','intent_source_changed')))
 OR EXISTS(SELECT 1 FROM review_execution_reservations r JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE a.operation_id=?)
 OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE operation_id=?)`, o.ID, o.ID, o.ID).Scan(&unsafe)
		if err != nil {
			return err
		}
		if unsafe {
			return core.Fail("review_operation_started", "An operation with possible external work cannot be rebound.", 409)
		}
		prior, err := readIntentLineage(ctx, tx, o.ProjectID, o.ID)
		if err != nil {
			return err
		}
		v := IntentLineage{RootOperationID: o.ID, PredecessorID: o.ID, IntentCreatedAt: o.CreatedAt, Depth: 1, Source: source}
		if prior != nil {
			v.RootOperationID = prior.RootOperationID
			v.IntentCreatedAt = prior.IntentCreatedAt
			v.NotBefore = prior.NotBefore
			v.CLIExhaustions = prior.CLIExhaustions
			v.SourceRetries = prior.SourceRetries
			v.Depth = prior.Depth + 1
		}
		if v.Depth > 1000 {
			return core.Fail("review_intent_rebind_limit", "This intent has reached its bounded source-generation history limit.", 409)
		}
		retainFloor := func(value string) error {
			if value == "" {
				return nil
			}
			due, ok := attemptRecordedTime(value)
			if !ok {
				return waitUnavailable()
			}
			if v.NotBefore == "" || due.After(mustWaitTime(v.NotBefore)) {
				v.NotBefore = value
			}
			return nil
		}
		if o.Wait != nil {
			if err = retainFloor(o.Wait.RetryNotBefore); err != nil {
				return err
			}
			if o.Channel == "hosted" {
				if err = retainFloor(o.Wait.hostedRetryNotBefore()); err != nil {
					return err
				}
			}
			if o.Wait.SourceRetry != nil {
				if err = retainFloor(o.Wait.SourceRetry.RetryNotBefore); err != nil {
					return err
				}
			}
		}
		var exhaustions, retries int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_execution_attempts a JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision WHERE a.operation_id=? AND t.reason='allowance_exhausted' LIMIT 3)`, o.ID).Scan(&exhaustions); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_source_retries WHERE operation_id=?`, o.ID).Scan(&retries); err != nil {
			return err
		}
		v.CLIExhaustions = min(3, v.CLIExhaustions+exhaustions)
		v.SourceRetries += retries
		if v.SourceRetries > sourceRetryLimit {
			return waitUnavailable()
		}
		at, err := attemptRecordingStamp(ctx, tx, o, nil, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		v.ReboundAt = at
		_, revision, err := readConfiguration(ctx, tx, o.ProjectID)
		if err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='cancelled',reason='source_superseded',revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state='queued' AND attempt_id=''`, at, o.ID, o.Revision)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return conflict()
		}
		receipt, err := e.enqueueRequestTx(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, Request{ExpectedConfigurationRevision: revision, PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: source.HeadSHA, AllowOutOfOrder: o.AllowOutOfOrder, OverrideFileLimit: o.OverrideFileLimit})
		if err != nil {
			return err
		}
		v.SuccessorID = receipt.ID
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET intent_predecessor_id=? WHERE id=?`, o.ID, receipt.ID); err != nil {
			return err
		}
		body, err := json.Marshal(source)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_intent_rebindings(successor_operation_id,predecessor_operation_id,root_operation_id,project_id,intent_created_at,not_before,cli_exhaustions,source_retries,depth,rebound_at,source_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.SuccessorID, v.PredecessorID, v.RootOperationID, o.ProjectID, v.IntentCreatedAt, v.NotBefore, v.CLIExhaustions, v.SourceRetries, v.Depth, v.ReboundAt, string(body))
		if err != nil {
			return err
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.intent_rebound", o.ID, o.Revision+1, map[string]string{"successor_operation_id": v.SuccessorID, "root_operation_id": v.RootOperationID, "reason": "current_comparison"}); err != nil {
			return err
		}
		result, err = readOperation(ctx, tx, o.ProjectID, v.SuccessorID)
		return err
	}()
	return result, err
}

// IntentSourceChanged is emitted only by trusted preparation adapters before
// an allowance query, review command or other external write. It is not an API
// request and never permits repeating an uncertain provider action.
type IntentSourceChanged struct{ Source SourceObservation }

func (*IntentSourceChanged) Error() string {
	return "review intent comparison changed before external action"
}

func (e *Engine) replacePreparingIntentOnce(ctx context.Context, operation Operation, attempt Attempt, source SourceObservation, retryableRollback *bool) (Operation, error) {
	var result Operation
	observed, valid := attemptRecordedTime(source.ObservedAt)
	now := e.sampleExecutionTime()
	if !validSourceObservation(source) || !valid || source.PullNumber != operation.PullNumber || observed.After(now) || now.Sub(observed) > time.Minute {
		return result, sourceHold("observation_incomplete")
	}
	entered, callbackFailed := false, false
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) (writeErr error) {
		entered = true
		defer func() { callbackFailed = writeErr != nil }()
		o, err := readOperation(ctx, tx, operation.ProjectID, operation.ID)
		if err != nil {
			return err
		}
		a, err := readAttempt(ctx, tx, operation.ProjectID, attempt.ID)
		if err != nil {
			return err
		}
		if o.State != "preparing" || o.AttemptID != a.ID || a.OperationID != o.ID || a.Revision != attempt.Revision || a.State != "preparing" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
			return conflict()
		}
		var queried bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_hosted_allowance_queries WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?)`, a.ID, a.ID).Scan(&queried); err != nil {
			return err
		}
		if queried {
			return core.Fail("review_operation_started", "Preparation has external history and cannot be rebound.", 409)
		}
		at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		a.State = "held"
		a.Revision++
		if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='held',revision=? WHERE id=?`, a.Revision, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason='intent_source_changed',revision=revision+1,updated_at=? WHERE id=?`, at, o.ID); err != nil {
			return err
		}
		if err = e.transitionRow(ctx, tx, a, "intent_source_changed", at); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason='',attempt_id='' WHERE id=?`, o.ID); err != nil {
			return err
		}
		o, err = readOperation(ctx, tx, o.ProjectID, o.ID)
		if err != nil {
			return err
		}
		result, err = e.rebindQueuedIntentTx(ctx, tx, o, source)
		return err
	})
	*retryableRollback = !entered || callbackFailed
	if err == nil {
		e.notifyOperation(operation.ID)
		e.notifyOperation(result.ID)
	}
	return result, err
}

func historicalIntentReplacement(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (bool, error) {
	var reason string
	if err := q.QueryRowContext(ctx, `SELECT reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&reason); err != nil {
		return false, err
	}
	if reason != "intent_source_changed" {
		return false, nil
	}
	if o.State != "cancelled" || o.Reason != "source_superseded" || a.State != "held" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || a.OperationID != o.ID || a.ProjectID != o.ProjectID {
		return false, waitUnavailable()
	}
	var successor string
	if err := q.QueryRowContext(ctx, `SELECT successor_operation_id FROM review_intent_rebindings WHERE predecessor_operation_id=? AND project_id=?`, o.ID, o.ProjectID).Scan(&successor); err != nil {
		return false, waitDependencyError(err)
	}
	if _, err := readIntentLineage(ctx, q, o.ProjectID, successor); err != nil {
		return false, err
	}
	var occupied bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_hosted_allowance_queries WHERE attempt_id=?)`, a.ID, a.ID, a.ID).Scan(&occupied); err != nil {
		return false, err
	}
	if occupied {
		return false, waitUnavailable()
	}
	return true, nil
}

// IntentProgress exposes scheduling lineage without leaking source branch or
// provider credential details. Attempt anchors remain the run authority.
type IntentProgress struct {
	RootOperationID string `json:"root_operation_id"`
	PredecessorID   string `json:"predecessor_operation_id,omitempty"`
	SuccessorID     string `json:"successor_operation_id,omitempty"`
	CreatedAt       string `json:"created_at"`
	NotBefore       string `json:"not_before,omitempty"`
	Generation      int    `json:"generation"`
}

func readIntentProgress(ctx context.Context, q EvidenceQuery, o Operation) (*IntentProgress, error) {
	v, err := readIntentLineage(ctx, q, o.ProjectID, o.ID)
	if err != nil {
		return nil, err
	}
	var progress *IntentProgress
	if v != nil {
		progress = &IntentProgress{RootOperationID: v.RootOperationID, PredecessorID: v.PredecessorID, CreatedAt: v.IntentCreatedAt, NotBefore: v.NotBefore, Generation: v.Depth + 1}
	}
	if o.Reason != "source_superseded" {
		return progress, nil
	}
	var next string
	if err = q.QueryRowContext(ctx, `SELECT successor_operation_id FROM review_intent_rebindings WHERE project_id=? AND predecessor_operation_id=?`, o.ProjectID, o.ID).Scan(&next); err != nil {
		return nil, waitDependencyError(err)
	}
	n, err := readIntentLineage(ctx, q, o.ProjectID, next)
	if err != nil {
		return nil, err
	}
	if progress == nil {
		progress = &IntentProgress{RootOperationID: n.RootOperationID, CreatedAt: n.IntentCreatedAt, Generation: 1}
	}
	progress.SuccessorID = next
	return progress, nil
}

func (e *Engine) replacePreparingIntent(ctx context.Context, operation Operation, attempt Attempt, source SourceObservation) (Operation, error) {
	local, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result Operation
	err := retryRunSelectionLocal(local, func() error {
		safeRetry := false
		next, err := e.replacePreparingIntentOnce(local, operation, attempt, source, &safeRetry)
		if err == nil {
			result = next
			return nil
		}
		// A returned commit error is not permission to create a second successor.
		// Recover only the exact durable source/attempt handoff with a fresh read.
		if recorded, ok := e.recordedIntentReplacement(local, operation, attempt, source); ok {
			result = recorded
			return nil
		}
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "overloaded" {
			if safeRetry {
				return err
			}
			return persistenceFailureAt("intent_rebind", err, operation.ID, attempt.ID)
		}
		return err
	})
	return result, err
}

func (e *Engine) recordedIntentReplacement(ctx context.Context, operation Operation, attempt Attempt, source SourceObservation) (Operation, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var result Operation
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		old, err := readOperation(ctx, tx, operation.ProjectID, operation.ID)
		if err != nil {
			return err
		}
		a, err := readAttempt(ctx, tx, operation.ProjectID, attempt.ID)
		if err != nil {
			return err
		}
		if old.HeadSHA != operation.HeadSHA || old.ActorID != operation.ActorID || old.WorkerID != operation.WorkerID || old.PullNumber != operation.PullNumber || old.Channel != operation.Channel || a.Revision != attempt.Revision+1 {
			return conflict()
		}
		valid, err := historicalIntentReplacement(ctx, tx, old, a)
		if err != nil {
			return err
		}
		if !valid {
			return conflict()
		}
		var id string
		if err = tx.QueryRowContext(ctx, `SELECT successor_operation_id FROM review_intent_rebindings WHERE predecessor_operation_id=? AND project_id=?`, old.ID, old.ProjectID).Scan(&id); err != nil {
			return err
		}
		v, err := readIntentLineage(ctx, tx, old.ProjectID, id)
		if err != nil {
			return err
		}
		if v == nil || v.Source != source {
			return conflict()
		}
		result, err = readOperation(ctx, tx, old.ProjectID, id)
		return err
	})
	return result, err == nil
}
