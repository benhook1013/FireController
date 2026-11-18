package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// cliAdmissionMargin treats still-live runs admitted shortly before an
// allowance observation as not yet reflected in it.
const cliAdmissionMargin = 5 * time.Minute

// CLICapacityBusyReason returns an unused CLI preparation to its queue when the
// account's parallel limit or observed allowance is already spoken for.
const CLICapacityBusyReason = "cli_capacity_busy"

// WithCLIParallelLimit sets how many CLI review processes one provider account
// may run at once. The observed provider allowance can lower it further.
func WithCLIParallelLimit(limit int) EngineOption {
	return func(e *Engine) {
		if limit >= 1 && limit <= 16 {
			e.cliParallelLimit = int64(limit)
		}
	}
}

func (e *Engine) cliLimit() int64 {
	if e.cliParallelLimit < 1 {
		return 1
	}
	return e.cliParallelLimit
}

// cliAccountFree returns how many more CLI runs the account may start. It is
// read in the caller's transaction; a write transaction makes it atomic.
func (e *Engine) cliAccountFree(ctx context.Context, q EvidenceQuery, provider, account string) (int64, error) {
	var live, preparing int64
	if err := q.QueryRowContext(ctx, `SELECT count(*),COALESCE(SUM(a.state='preparing'),0) FROM review_execution_reservations r JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE r.provider=? AND r.account=? AND r.action_class='cli' AND r.execution_ended_at=''`, provider, account).Scan(&live, &preparing); err != nil {
		return 0, err
	}
	free := e.cliLimit() - live
	if free <= 0 {
		return 0, nil
	}
	var remaining sql.NullInt64
	var observed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT json_extract(p.body_json,'$.observation.remaining'),json_extract(p.body_json,'$.observation.observed_at') FROM review_cli_preflight p JOIN review_execution_attempts a ON a.id=p.attempt_id WHERE a.provider=? AND a.account=? AND json_valid(p.body_json) AND json_type(p.body_json,'$.observation.remaining')='integer' AND json_type(p.body_json,'$.observation.observed_at')='text' AND julianday(json_extract(p.body_json,'$.observation.observed_at')) IS NOT NULL ORDER BY julianday(json_extract(p.body_json,'$.observation.observed_at')) DESC LIMIT 1`, provider, account).Scan(&remaining, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return free, nil
	}
	if err != nil {
		return 0, err
	}
	// One PR's damaged observation must not stop other PRs: ignore it.
	at, parseErr := time.Parse(time.RFC3339Nano, observed.String)
	if !remaining.Valid || !observed.Valid || parseErr != nil {
		return free, nil
	}
	admitted, err := cliAdmittedSince(ctx, q, provider, account, "", at)
	if err != nil {
		return 0, err
	}
	quota := remaining.Int64 - admitted - preparing
	// With nothing running, one preparation may refresh a stale observation;
	// its own allowance check still refuses an exhausted account.
	if live == 0 && quota < 1 {
		quota = 1
	}
	return min(free, quota), nil
}

func cliAdmittedSince(ctx context.Context, q EvidenceQuery, provider, account, except string, at time.Time) (int64, error) {
	var count int64
	// Runs admitted after the observation are unreflected. A run admitted just
	// before it may not have spawned yet, so count it too while still live.
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_transitions t JOIN review_execution_attempts a ON a.id=t.attempt_id JOIN review_operations o ON o.id=a.operation_id WHERE t.reason='admitted' AND o.channel='cli' AND a.provider=? AND a.account=? AND a.id<>? AND (julianday(t.created_at)>julianday(?) OR julianday(t.created_at)>julianday(?) AND EXISTS(SELECT 1 FROM review_execution_reservations r WHERE r.attempt_id=a.id AND r.action_class='cli' AND r.execution_ended_at=''))`, provider, account, except, at.UTC().Format(time.RFC3339Nano), at.Add(-cliAdmissionMargin).UTC().Format(time.RFC3339Nano)).Scan(&count)
	return count, err
}

// requireCLIAdmissionCapacity is the atomic final check before a CLI attempt
// becomes active: the parallel limit and the attempt's own allowance
// observation must both leave room after every other admitted run.
func (e *Engine) requireCLIAdmissionCapacity(ctx context.Context, tx *sql.Tx, o Operation, a Attempt) error {
	busy := core.Fail(CLICapacityBusyReason, "Other CLI reviews already use this account's parallel limit or allowance; the request stays queued.", 409)
	var active int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_reservations r JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE r.provider=? AND r.account=? AND r.action_class='cli' AND r.execution_ended_at='' AND a.state<>'preparing' AND a.id<>?`, a.Provider, a.Account, a.ID).Scan(&active); err != nil {
		return err
	}
	if active >= e.cliLimit() {
		return busy
	}
	record, err := readCLIAllowanceRecord(ctx, tx, o, a)
	if errors.Is(err, errAllowanceObservationAbsent) {
		// Only the parallel limit applies without an observation; the trusted
		// CLI driver always records one during preparation.
		return nil
	}
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, record.Observation.ObservedAt)
	if err != nil {
		return allowanceUnavailable()
	}
	admitted, err := cliAdmittedSince(ctx, tx, a.Provider, a.Account, a.ID, at)
	if err != nil {
		return err
	}
	if admitted >= record.Observation.Remaining {
		return busy
	}
	return nil
}
