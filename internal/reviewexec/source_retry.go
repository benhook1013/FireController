package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const sourceRetryReason = "source_rate_limit_wait"
const sourceRetryLimit = 3

// RFC3339Nano omits trailing zeros; SQLite julianday loses submillisecond
// precision. Pad the canonical fractional part for exact indexed floor order.
const sourceRetryOrder = `substr(retry_not_before,1,19) DESC,CASE WHEN substr(retry_not_before,20,1)='.' THEN substr(substr(retry_not_before,21,length(retry_not_before)-21)||'000000000',1,9) ELSE '000000000' END DESC`

// SourceRetry is retained read-only preparation evidence, never review allowance.
// It exposes neither the source credential group nor any provider response text.
type SourceRetry struct {
	AttemptID      string `json:"attempt_id"`
	ObservedAt     string `json:"observed_at"`
	RetryNotBefore string `json:"retry_not_before"`
	Sequence       int    `json:"sequence"`
}

// WithSourceReadGroups binds installed capabilities sharing the exact same
// GitHub credential. Values are private opaque hashes, not review account names.
// It is trusted runtime composition, never a client-controlled input.
func WithSourceReadGroups(groups map[string]string) EngineOption {
	copy := make(map[string]string, len(groups))
	for key, group := range groups {
		copy[key] = group
	}
	return func(e *Engine) { e.sourceReadGroups = copy }
}

func sourceRetrySchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_source_retries", []string{
		`CREATE TABLE review_source_retries(attempt_id TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),operation_id TEXT NOT NULL REFERENCES review_operations(id),source_group TEXT NOT NULL,observed_at TEXT NOT NULL,retry_not_before TEXT NOT NULL,sequence INTEGER NOT NULL CHECK(sequence BETWEEN 1 AND 3),UNIQUE(operation_id,sequence))`,
		`CREATE INDEX review_source_retries_group ON review_source_retries(source_group,` + sourceRetryOrder + `)`,
		`CREATE INDEX review_source_retries_due ON review_source_retries(julianday(retry_not_before))`,
	}, []string{`ALTER TABLE review_source_retries ADD COLUMN timing_state TEXT NOT NULL DEFAULT 'proven' CHECK(timing_state IN ('proven','conservative'))`})
}

func sourceRetriesPresent(ctx context.Context, q EvidenceQuery) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_source_retries'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if version != "1" && version != "2" {
		return false, waitUnavailable()
	}
	return true, nil
}

func sourceRetryTimingState(ctx context.Context, q EvidenceQuery, attempt string) (string, error) {
	var version string
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_source_retries'`).Scan(&version); err != nil {
		return "", err
	}
	if version == "1" {
		return "proven", nil
	}
	var state string
	err := q.QueryRowContext(ctx, `SELECT timing_state FROM review_source_retries WHERE attempt_id=?`, attempt).Scan(&state)
	if err == nil && state != "proven" && state != "conservative" {
		return "", waitUnavailable()
	}
	return state, err
}

func readSourceRetry(ctx context.Context, q EvidenceQuery, o Operation) (*SourceRetry, error) {
	present, err := sourceRetriesPresent(ctx, q)
	if err != nil || !present {
		return nil, err
	}
	var latest *SourceRetry
	var originalGroup string
	for sequence := 1; sequence <= sourceRetryLimit+1; sequence++ {
		var r SourceRetry
		var group string
		err := q.QueryRowContext(ctx, `SELECT attempt_id,source_group,observed_at,retry_not_before,sequence FROM review_source_retries WHERE operation_id=? AND sequence=?`, o.ID, sequence).Scan(&r.AttemptID, &group, &r.ObservedAt, &r.RetryNotBefore, &r.Sequence)
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM review_source_retries WHERE operation_id=?`, o.ID).Scan(&count); err != nil {
				return nil, err
			}
			if count != sequence-1 {
				return nil, waitUnavailable()
			}
			break
		}
		if err != nil {
			return nil, err
		}
		if sequence > sourceRetryLimit || !hash.MatchString(group) || originalGroup != "" && originalGroup != group {
			return nil, waitUnavailable()
		}
		originalGroup = group
		a, err := readAttempt(ctx, q, o.ProjectID, r.AttemptID)
		if err != nil {
			return nil, err
		}
		if err := validateSourceRetry(ctx, q, o, a, r); err != nil {
			return nil, err
		}
		if latest != nil && mustWaitTime(r.ObservedAt).Before(mustWaitTime(latest.RetryNotBefore)) {
			return nil, waitUnavailable()
		}
		latest = &r
	}
	return latest, nil
}

func validateSourceRetry(ctx context.Context, q EvidenceQuery, o Operation, a Attempt, r SourceRetry) error {
	state, stateErr := sourceRetryTimingState(ctx, q, r.AttemptID)
	if stateErr != nil {
		return stateErr
	}
	if state == "conservative" && mustWaitTime(r.RetryNotBefore).Sub(mustWaitTime(r.ObservedAt)) != 15*time.Minute {
		return waitUnavailable()
	}
	observed, validObserved := attemptRecordedTime(r.ObservedAt)
	due, validDue := attemptRecordedTime(r.RetryNotBefore)
	if !validChannel(o.Channel) || a.ID != r.AttemptID || a.OperationID != o.ID || a.ProjectID != o.ProjectID || a.ID == o.AttemptID || a.State != "held" || a.Revision != 2 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || !validObserved || !validDue || !due.After(observed) || due.Sub(observed) > 24*time.Hour || r.Sequence < 1 || r.Sequence > sourceRetryLimit {
		return waitUnavailable()
	}
	var start, ended string
	var occupied bool
	err := q.QueryRowContext(ctx, `SELECT first.created_at,last.created_at,
 EXISTS(SELECT 1 FROM review_execution_transitions WHERE attempt_id=first.attempt_id AND revision NOT IN(1,2))
 OR EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=first.attempt_id)
 OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=first.attempt_id)
 OR EXISTS(SELECT 1 FROM review_cli_preflight WHERE attempt_id=first.attempt_id)
 OR EXISTS(SELECT 1 FROM review_hosted_preflight WHERE attempt_id=first.attempt_id)
 OR EXISTS(SELECT 1 FROM review_hosted_allowance_queries WHERE attempt_id=first.attempt_id)
 FROM review_execution_transitions first JOIN review_execution_transitions last ON last.attempt_id=first.attempt_id AND last.revision=2
 WHERE first.attempt_id=? AND first.revision=1 AND first.state='preparing' AND first.reason='preparing' AND first.external_id=''
 AND last.state='held' AND last.reason=? AND last.external_id=''`, a.ID, sourceRetryReason).Scan(&start, &ended, &occupied)
	began, validStart := attemptRecordedTime(start)
	finished, validEnd := attemptRecordedTime(ended)
	if err != nil || occupied || !validStart || !validEnd || observed.Before(began) || finished.Before(observed) || !due.After(finished) {
		return waitUnavailable()
	}
	return nil
}

func historicalSourceRetryOperation(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (Operation, bool, error) {
	present, err := sourceRetriesPresent(ctx, q)
	if err != nil || !present {
		return Operation{}, false, err
	}
	var r SourceRetry
	err = q.QueryRowContext(ctx, `SELECT attempt_id,observed_at,retry_not_before,sequence FROM review_source_retries WHERE attempt_id=? AND operation_id=?`, a.ID, o.ID).Scan(&r.AttemptID, &r.ObservedAt, &r.RetryNotBefore, &r.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	if err := validateSourceRetry(ctx, q, o, a, r); err != nil {
		return Operation{}, false, err
	}
	o.AttemptID, o.State, o.Reason, o.Wait = a.ID, "held", sourceRetryReason, nil
	return o, true, nil
}

// SourceReadGate is consulted by every new GitHub read for the same installed
// credential, including observation lanes. No network call or sleep occurs here.
func (e *Engine) SourceReadGate(ctx context.Context, group string) error {
	if !hash.MatchString(group) {
		return invalid()
	}
	var retry *SourceRetry
	var timingState string
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		retry, err = sourceGroupRetry(ctx, tx, group)
		if err == nil && retry != nil {
			timingState, err = sourceRetryTimingState(ctx, tx, retry.AttemptID)
		}
		return err
	})
	if err != nil {
		return err
	}
	if retry != nil && e.sampleExecutionTime().Before(mustWaitTime(retry.RetryNotBefore)) {
		timing := reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: mustWaitTime(retry.ObservedAt), NotBefore: mustWaitTime(retry.RetryNotBefore)}
		if timingState == "conservative" {
			timing.State, timing.NotBefore = "unknown", time.Time{}
		}
		return &reviewprovider.Error{Code: "provider_rate_limited", Message: "Source reads are waiting for a retained retry deadline; availability is not established.", Status: http.StatusTooManyRequests, RetryTiming: timing}
	}
	return nil
}

func sourceGroupRetry(ctx context.Context, q EvidenceQuery, group string) (*SourceRetry, error) {
	present, err := sourceRetriesPresent(ctx, q)
	if err != nil || !present {
		return nil, err
	}
	var project, id string
	err = q.QueryRowContext(ctx, `SELECT o.project_id,o.id FROM review_source_retries r JOIN review_operations o ON o.id=r.operation_id WHERE r.source_group=? ORDER BY `+sourceRetryOrder+` LIMIT 1`, group).Scan(&project, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := readOperationBase(ctx, q, project, id)
	if err != nil {
		return nil, err
	}
	return readSourceRetry(ctx, q, o)
}

func (e *Engine) sourceCandidateDue(ctx context.Context, q EvidenceQuery, o Operation, c Configuration) (bool, error) {
	if o.Wait != nil && o.Wait.SourceRetry != nil && e.sampleExecutionTime().Before(mustWaitTime(o.Wait.SourceRetry.RetryNotBefore)) {
		return false, nil
	}
	group := e.sourceReadGroups[c.CapabilityID]
	if group == "" {
		return true, nil
	}
	retry, err := sourceGroupRetry(ctx, q, group)
	if err != nil {
		return false, err
	}
	return retry == nil || !e.sampleExecutionTime().Before(mustWaitTime(retry.RetryNotBefore)), nil
}

// ScheduleSourceRetry atomically retains an unused CLI or Hosted preparation after a rate-limited GitHub source read.
// False means ineligible or exhausted: the supervisor must settle normally.
// This API accepts only the typed runtime preparation failure, never wire input.
func (e *Engine) ScheduleSourceRetry(ctx context.Context, project, attemptID string, revision int64, failure error) (bool, error) {
	var stage *preparationFailure
	var provider *reviewprovider.Error
	if !errors.As(failure, &stage) || (stage.stage != "source_resolution" && stage.stage != "source_refresh") || !errors.As(stage.err, &provider) || provider.Code != "provider_rate_limited" || (provider.Status != 429 && provider.Status != 403) || provider.OutcomeUnknown || (provider.RetryTiming.State != "proven" && provider.RetryTiming.State != "unknown") {
		return false, nil
	}
	timing := provider.RetryTiming
	conservative := timing.State == "unknown"
	// A qualified refusal without timing uses an explicit local retry policy, not a fabricated provider reset.
	if conservative {
		if !timing.NotBefore.IsZero() {
			return false, nil
		}
		timing.NotBefore = timing.ObservedAt.Add(15 * time.Minute)
	}
	if timing.ObservedAt.IsZero() || !timing.NotBefore.After(timing.ObservedAt) || timing.NotBefore.Sub(timing.ObservedAt) > 24*time.Hour {
		return false, nil
	}
	scheduled := false
	var operationID string
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperationBase(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		operationID = o.ID
		// Exact replay after a committed local settlement never creates another row.
		if a.Revision == revision+1 && a.State == "held" {
			var observed, due, state string
			err := tx.QueryRowContext(ctx, `SELECT observed_at,retry_not_before,timing_state FROM review_source_retries WHERE attempt_id=? AND operation_id=?`, a.ID, o.ID).Scan(&observed, &due, &state)
			if err == nil && (state == "conservative") == conservative && observed == timing.ObservedAt.UTC().Format(time.RFC3339Nano) && due == timing.NotBefore.UTC().Format(time.RFC3339Nano) {
				scheduled = true
				return nil
			}
			return conflict()
		}
		now := e.sampleExecutionTime()
		if timing.ObservedAt.After(now) || !timing.NotBefore.After(now) {
			return nil
		}
		cliSourceRead := o.Channel == "cli" && stage.stage == "source_resolution"
		hostedRefresh := o.Channel == "hosted" && (stage.stage == "source_resolution" || stage.stage == "source_refresh")
		if (!cliSourceRead && !hostedRefresh) || a.Revision != revision || a.Revision != 1 || a.State != "preparing" || o.State != "preparing" || o.AttemptID != a.ID || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || o.Revision == math.MaxInt64 {
			return nil
		}
		var c Configuration
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, o.ProjectID, o.ConfigurationRevision).Scan(&body); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			return err
		}
		group := e.sourceReadGroups[c.CapabilityID]
		if !hash.MatchString(group) {
			return nil
		}
		previous, err := readSourceRetry(ctx, tx, o)
		if err != nil {
			return err
		}
		sequence := 1
		if previous != nil {
			sequence = previous.Sequence + 1
		}
		lineage, err := readIntentLineage(ctx, tx, o.ProjectID, o.ID)
		if err != nil {
			return err
		}
		inheritedRetries := 0
		if lineage != nil {
			inheritedRetries = lineage.SourceRetries
		}
		if sequence+inheritedRetries > sourceRetryLimit {
			return nil
		}
		at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
		if err != nil {
			return err
		}
		if !timing.NotBefore.After(mustWaitTime(at)) || timing.ObservedAt.After(mustWaitTime(at)) {
			return nil
		}
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_cli_preflight WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_hosted_preflight WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_hosted_allowance_queries WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?)`, a.ID, a.ID, a.ID, a.ID).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return nil
		}
		var started string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID).Scan(&started); err != nil {
			return err
		}
		if timing.ObservedAt.Before(mustWaitTime(started)) {
			return nil
		}
		a.State, a.Revision = "held", 2
		if _, err := tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='held',revision=2 WHERE id=?`, a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason=?,revision=revision+1,updated_at=? WHERE id=?`, sourceRetryReason, at, o.ID); err != nil {
			return err
		}
		// Project while still attached, before introducing historical wait evidence.
		if err := e.transitionRow(ctx, tx, a, sourceRetryReason, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_source_retries(attempt_id,operation_id,source_group,observed_at,retry_not_before,sequence,timing_state) VALUES(?,?,?,?,?,?,?)`, a.ID, o.ID, group, timing.ObservedAt.UTC().Format(time.RFC3339Nano), timing.NotBefore.UTC().Format(time.RFC3339Nano), sequence, func() string {
			if conservative {
				return "conservative"
			}
			return "proven"
		}()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason=?,attempt_id='' WHERE id=?`, sourceRetryReason, o.ID); err != nil {
			return err
		}
		if err := core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
			return err
		}
		scheduled = true
		return nil
	})
	if err == nil && scheduled {
		e.notifyOperation(operationID)
	}
	return scheduled, err
}

// ValidateSourceRetriesSnapshot rejects fabricated clocks, detached ownership,
// missing evidence and changed history before a backup may be restored.
func ValidateSourceRetriesSnapshot(ctx context.Context, tx *sql.Tx) error {
	present, err := sourceRetriesPresent(ctx, tx)
	if err != nil || !present {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT o.project_id,r.operation_id FROM review_source_retries r LEFT JOIN review_operations o ON o.id=r.operation_id UNION SELECT project_id,id FROM review_operations WHERE reason=? UNION SELECT a.project_id,a.operation_id FROM review_execution_attempts a JOIN review_execution_transitions t ON t.attempt_id=a.id WHERE t.reason=?`, sourceRetryReason, sourceRetryReason)
	if err != nil {
		return err
	}
	var owners [][2]string
	for rows.Next() {
		var owner [2]string
		if err := rows.Scan(&owner[0], &owner[1]); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, owner := range owners {
		o, err := readOperationBase(ctx, tx, owner[0], owner[1])
		if err != nil {
			return err
		}
		r, err := readSourceRetry(ctx, tx, o)
		if err != nil {
			return err
		}
		if r == nil {
			return waitUnavailable()
		}
		if o.Reason == sourceRetryReason && (o.State != "queued" || o.AttemptID != "") {
			return waitUnavailable()
		}
		if err := validateOperationWaitHistory(ctx, tx, o); err != nil {
			return err
		}
	}
	return nil
}
