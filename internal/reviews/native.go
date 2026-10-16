package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const nativeSource = "native-executor"

// NativePublication is trusted in-process evidence, not an import request.
// Pull contains only the selected PR's metadata; existing history is retained.
// Run appends new captured evidence and never replaces an existing source run.
type NativePublication struct {
	Pull    PullInput
	Attempt *Attempt
	Run     *RunInput
}

// PublishNative advances the immutable browser projection inside the caller's
// journal transaction. It copies stored rows without relabeling their origin
// or IDs and adds only this native update. It performs no external work or
// administrator import, and never exposes raw captures through queue reads.
func PublishNative(ctx context.Context, tx *sql.Tx, scope core.Scope, p NativePublication) (Receipt, error) {
	if len(p.Pull.Runs) != 0 || len(p.Pull.Attempts) != 0 || p.Attempt == nil && p.Run == nil {
		return Receipt{}, validationError("Publish one selected native attempt or run without replacing history.")
	}
	var repositoryID int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repositoryID); err != nil {
		return Receipt{}, err
	}
	var previousID, previousObserved string
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT w.revision,w.snapshot_id,s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, scope.ProjectID).Scan(&revision, &previousID, &previousObserved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	if revision == int64(^uint64(0)>>1) {
		return Receipt{}, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	observed := previousObserved
	if observed == "" {
		observed = time.Now().UTC().Format(time.RFC3339Nano)
	}
	// The overall observation time stays conservative when only one PR is
	// refreshed. Recording a new attempt does not refresh every other PR.
	pull := p.Pull
	if p.Run != nil {
		pull.Runs = []RunInput{*p.Run}
	}
	if p.Attempt != nil {
		a := *p.Attempt
		a.SourceRunID = ""
		pull.Attempts = []Attempt{a}
	}
	request := ObservationRequest{ExpectedRevision: &revision, RepositoryID: repositoryID, Source: nativeSource, ObservedAt: observed, Queue: []PullInput{pull}}
	if err = validateObservation(&request); err != nil {
		return Receipt{}, err
	}
	pull = request.Queue[0]
	if p.Run != nil {
		if err = validateFinalizedCounts(ctx, tx, scope.ProjectID, nativeSource, []PullInput{pull}); err != nil {
			return Receipt{}, err
		}
	}
	id := core.NewID("review_snapshot")
	next := revision + 1
	fingerprint := sha256.Sum256([]byte(previousID + "\x00" + mustJSON(p)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, repositoryID, nativeSource, observed, time.Now().UTC().Format(time.RFC3339Nano), hex.EncodeToString(fingerprint[:])); err != nil {
		return Receipt{}, err
	}
	if previousID != "" {
		if err = copyNativeSnapshot(ctx, tx, scope.ProjectID, previousID, id); err != nil {
			return Receipt{}, err
		}
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, scope.ProjectID, id, pull.Number).Scan(&exists); err != nil {
		return Receipt{}, err
	}
	if exists == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_pulls WHERE project_id=? AND snapshot_id=?`, scope.ProjectID, id).Scan(&count); err != nil {
			return Receipt{}, err
		}
		if count >= maxQueuePulls {
			return Receipt{}, validationError("The native workspace queue is at its current capacity.")
		}
		metadata := pull
		metadata.Runs = nil
		metadata.Attempts = nil
		if err = storePull(ctx, tx, scope.ProjectID, id, nativeSource, count, metadata); err != nil {
			return Receipt{}, err
		}
	} else {
		// Native metadata does not assert a freshly complete inventory. Preserve
		// that source fact only while it still describes the same head.
		if _, err = tx.ExecContext(ctx, `UPDATE review_pulls SET title=?,state=?,complete=CASE WHEN head_sha=? THEN complete ELSE 0 END,head_sha=?,base_ref=?,base_sha=?,ci_state=?,mergeability=? WHERE project_id=? AND snapshot_id=? AND number=?`, pull.Title, pull.State, pull.HeadSHA, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, scope.ProjectID, id, pull.Number); err != nil {
			return Receipt{}, err
		}
	}
	if p.Run != nil {
		if err = appendNativeRun(ctx, tx, scope.ProjectID, id, pull.Number, pull.Runs[0]); err != nil {
			return Receipt{}, err
		}
	}
	if p.Attempt != nil {
		if err = publishNativeAttempt(ctx, tx, scope.ProjectID, id, pull.Number, *p.Attempt); err != nil {
			return Receipt{}, err
		}
	}
	if err = checkNativeCapacity(ctx, tx, scope.ProjectID, id); err != nil {
		return Receipt{}, err
	}
	if p.Run != nil {
		if err = checkNativeRunDetail(ctx, tx, scope.ProjectID, id, runOpaqueID(scope.ProjectID, nativeSource, p.Run.SourceRunID)); err != nil {
			return Receipt{}, err
		}
	}
	// Rebuild every compact projection from original rows. A copied pre-native
	// JSON summary may lack per-run source; its new mixed snapshot must not
	// use the publisher's label as a substitute for the original producer.
	if err = rebuildNativeSummaries(ctx, tx, scope.ProjectID, id, pull.Number); err != nil {
		return Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_workspace(project_id,revision,snapshot_id) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,snapshot_id=excluded.snapshot_id`, scope.ProjectID, next, id); err != nil {
		return Receipt{}, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "pull_number": pull.Number, "source": nativeSource}); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "observed"}, nil
}

func copyNativeSnapshot(ctx context.Context, tx *sql.Tx, project, previous, next string) error {
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, project, next).Scan(&revision); err != nil {
		return err
	}
	for _, table := range []struct{ name, columns string }{
		{"review_pulls", "position,number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json"},
		{"review_runs", "pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json"},
		{"review_findings", "pull_number,run_id,position,source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at"},
		{"review_attempts", "pull_number,position,source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id"},
		{"review_retired_pulls", "position,number,reason"},
	} {
		columns, selection := table.columns, "project_id,?,"+table.columns
		args := []any{next, project, previous}
		if table.name == "review_runs" {
			// A copy belongs to the new snapshot, not the retained source row's
			// older revision. The original row is never rewritten.
			columns = "snapshot_revision," + columns
			selection = "project_id,?,?," + table.columns
			args = []any{next, revision, project, previous}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table.name+`(project_id,snapshot_id,`+columns+`) SELECT `+selection+` FROM `+table.name+` WHERE project_id=? AND snapshot_id=?`, args...); err != nil {
			return err
		}
	}
	return nil
}

func appendNativeRun(ctx context.Context, tx *sql.Tx, project, snapshot string, number int64, run RunInput) error {
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_runs WHERE project_id=? AND source=? AND source_run_id=?`, project, nativeSource, run.SourceRunID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return core.Fail("observation_conflict", "A native captured run already exists; append decisions without replacing source evidence.", 409)
	}
	var order int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(run_order),-1)+1 FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=?`, project, snapshot, number).Scan(&order); err != nil {
		return err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, project, snapshot).Scan(&revision); err != nil {
		return err
	}
	summary := summarizeRun(project, nativeSource, number, run)
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_runs(project_id,snapshot_id,snapshot_revision,pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, project, snapshot, revision, number, order, summary.ID, nativeSource, run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete, run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, summary.Fixed, mustJSON(run.CoverageLimits)); err != nil {
		return err
	}
	for i, f := range run.Findings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings(project_id,snapshot_id,pull_number,run_id,position,source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, project, snapshot, number, summary.ID, i, f.SourceFindingID, f.Title, f.Detail, f.Severity, f.Disposition, f.DecisionReason, f.Actor, f.DecidedAt, f.FixSHA, f.ProofNote, f.ResolvedAt); err != nil {
			return err
		}
	}
	return nil
}

func publishNativeAttempt(ctx context.Context, tx *sql.Tx, project, snapshot string, number int64, a Attempt) error {
	if a.SourceRunID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? AND source=? AND source_run_id=? AND channel=? AND head_sha=?`, project, snapshot, number, nativeSource, a.SourceRunID, a.Channel, a.HeadSHA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return validationError("A native attempt must link to its own captured run on this PR.")
		}
	}
	var oldNumber int64
	var position int
	var oldRun, oldTrigger, oldProvider, oldState string
	err := tx.QueryRowContext(ctx, `SELECT pull_number,position,source_run_id,trigger_id,provider_review_id,state FROM review_attempts WHERE project_id=? AND snapshot_id=? AND source=? AND source_attempt_id=?`, project, snapshot, nativeSource, a.SourceAttemptID).Scan(&oldNumber, &position, &oldRun, &oldTrigger, &oldProvider, &oldState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && oldNumber != number {
		return core.Fail("observation_conflict", "A native attempt cannot move to another PR.", 409)
	}
	if err == nil {
		for _, identity := range [][2]string{{oldRun, a.SourceRunID}, {oldTrigger, a.TriggerID}, {oldProvider, a.ProviderReviewID}} {
			if identity[0] != "" && identity[0] != identity[1] {
				return core.Fail("observation_conflict", "Known native capture and provider identities cannot be cleared or replaced.", 409)
			}
		}
		if oneOf(oldState, "completed", "failed", "cancelled", "rate_limited", "held") && a.State != oldState {
			return core.Fail("observation_conflict", "A settled native attempt cannot be reopened.", 409)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_attempts WHERE project_id=? AND snapshot_id=? AND pull_number=?`, project, snapshot, number).Scan(&position); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_attempts(project_id,snapshot_id,pull_number,position,source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, project, snapshot, number, position, nativeSource, a.SourceAttemptID, a.Channel, a.HeadSHA, a.State, a.StartedAt, a.FinishedAt, a.DurationSeconds, a.SourceRunID, a.TriggerID, a.ProviderReviewID)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE review_attempts SET state=?,finished_at=?,duration_seconds=?,source_run_id=?,trigger_id=?,provider_review_id=? WHERE project_id=? AND snapshot_id=? AND source=? AND source_attempt_id=? AND channel=? AND head_sha=? AND started_at=?`, a.State, a.FinishedAt, a.DurationSeconds, a.SourceRunID, a.TriggerID, a.ProviderReviewID, project, snapshot, nativeSource, a.SourceAttemptID, a.Channel, a.HeadSHA, a.StartedAt)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				return core.Fail("observation_conflict", "Native attempt source identity cannot change.", 409)
			}
		}
	}
	return err
}

func checkNativeCapacity(ctx context.Context, tx *sql.Tx, project, snapshot string) error {
	for _, check := range []struct {
		table string
		max   int
		group string
		per   int
	}{
		{"review_runs", maxSnapshotRuns, "pull_number", maxPullRuns},
		{"review_attempts", maxSnapshotAttempts, "pull_number", maxPullAttempts},
		{"review_findings", maxSnapshotFindings, "run_id", maxRunFindings},
	} {
		var count, largest int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+check.table+` WHERE project_id=? AND snapshot_id=?`, project, snapshot).Scan(&count); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(n),0) FROM (SELECT count(*) AS n FROM `+check.table+` WHERE project_id=? AND snapshot_id=? GROUP BY `+check.group+`)`, project, snapshot).Scan(&largest); err != nil {
			return err
		}
		if count > check.max || largest > check.per {
			return core.Fail("workspace_capacity", "The retained review projection is at its current capacity; no history was dropped.", 409)
		}
	}
	return nil
}

func rebuildNativeSummaries(ctx context.Context, tx *sql.Tx, project, snapshot string, selectedPull int64) error {
	numbers, err := pullNumbersForSnapshot(ctx, tx, project, snapshot)
	if err != nil {
		return err
	}
	for _, number := range numbers {
		runs := []RunSummary{}
		rows, err := tx.QueryContext(ctx, `SELECT id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? ORDER BY run_order DESC LIMIT 3`, project, snapshot, number)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r RunSummary
			var limits string
			r.PullNumber = number
			if err = rows.Scan(&r.ID, &r.Source, &r.SourceRunID, &r.Channel, &r.HeadSHA, &r.Reviewer, &r.Scope, &r.Outcome, &r.Attributable, &r.Complete, &r.StartedAt, &r.FinishedAt, &r.Found, &r.Accepted, &r.Routed, &r.Finalized, &r.Fixed, &limits); err != nil {
				_ = rows.Close()
				return err
			}
			if err = json.Unmarshal([]byte(limits), &r.CoverageLimits); err != nil {
				_ = rows.Close()
				return err
			}
			runs = append(runs, r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		attempts := []Attempt{}
		rows, err = tx.QueryContext(ctx, `SELECT source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id FROM review_attempts WHERE project_id=? AND snapshot_id=? AND pull_number=? ORDER BY position`, project, snapshot, number)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a Attempt
			if err = rows.Scan(&a.SourceAttemptID, &a.Channel, &a.HeadSHA, &a.State, &a.StartedAt, &a.FinishedAt, &a.DurationSeconds, &a.SourceRunID, &a.TriggerID, &a.ProviderReviewID); err != nil {
				_ = rows.Close()
				return err
			}
			attempts = append(attempts, a)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		var runCount, incomplete int
		if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN complete=0 THEN 1 ELSE 0 END),0) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=?`, project, snapshot, number).Scan(&runCount, &incomplete); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_pulls SET run_count=?,attempt_count=?,complete=CASE WHEN ? THEN complete ELSE 0 END,recent_runs_json=?,latest_attempts_json=? WHERE project_id=? AND snapshot_id=? AND number=?`, runCount, len(attempts), number != selectedPull || incomplete == 0, mustJSON(runs), mustJSON(latestAttempts(attempts)), project, snapshot, number)
		if err != nil {
			return err
		}
	}
	return nil
}

// NativeAttemptCapacity reports whether appending one attempt to the current
// snapshot fits retained attempt bounds and, for a missing target, the pull-row
// bound. It does not reserve a slot or mutate history; callers must check and
// append in the same serialized write.
func NativeAttemptCapacity(ctx context.Context, tx *sql.Tx, project string, pullNumber int64) (bool, error) {
	var total, local int
	err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN a.pull_number=? THEN 1 ELSE 0 END),0)
 FROM review_workspace w JOIN review_attempts a ON a.project_id=w.project_id AND a.snapshot_id=w.snapshot_id WHERE w.project_id=?`, pullNumber, project).Scan(&total, &local)
	if err != nil {
		return false, err
	}
	var pulls, selected int
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN p.number=? THEN 1 ELSE 0 END),0)
 FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=?`, pullNumber, project).Scan(&pulls, &selected)
	return err == nil && total < maxSnapshotAttempts && local < maxPullAttempts && (selected != 0 || pulls < maxQueuePulls), err
}
