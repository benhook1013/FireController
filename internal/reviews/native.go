package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
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
// journal transaction. It retains prior versions without relabeling their origin
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
		for _, finding := range pull.Runs[0].Findings {
			if finding.Disposition == "routed" && !validFindingRoute(finding.Route, pull.Number, true) || finding.Disposition != "routed" && finding.Route != nil {
				return Receipt{}, validationError("Native routed findings require their active receiving route.")
			}
		}
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
	write := newHistorySnapshotWrite(scope.ProjectID, next)
	body, position, err := nativePullBody(ctx, tx, scope.ProjectID, previousID, pull)
	if err != nil {
		return Receipt{}, err
	}
	if p.Run != nil {
		if err = appendNativeRun(ctx, tx, write, id, pull.Number, pull.Runs[0]); err != nil {
			return Receipt{}, err
		}
	}
	if p.Attempt != nil {
		if err = publishNativeAttempt(ctx, tx, write, previousID, id, pull.Number, *p.Attempt); err != nil {
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
	// Only the selected PR's compact projection changes. Unrelated bodies and
	// original producers remain referenced directly, never recopied/relabelled.
	if err = rebuildNativeSummary(ctx, tx, write, id, pull.Number, position, body); err != nil {
		return Receipt{}, err
	}
	if err = write.applyCurrent(ctx, tx, id, revision); err != nil {
		return Receipt{}, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "pull_number": pull.Number, "source": nativeSource}); err != nil {
		return Receipt{}, err
	}
	if err = publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "observed"}, nil
}

func retainedPullBody(ctx context.Context, tx *sql.Tx, project, previous string, number int64) ([]any, int, error) {
	body := make([]any, len(historyPullRecord.body))
	targets := make([]any, 0, len(body)+1)
	for i := range body {
		targets = append(targets, &body[i])
	}
	var position int
	targets = append(targets, &position)
	err := tx.QueryRowContext(ctx, "SELECT "+strings.Join(historyPullRecord.body, ",")+",position FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?", project, previous, number).Scan(targets...)
	return body, position, err
}

func nativePullBody(ctx context.Context, tx *sql.Tx, project, previous string, pull PullInput) ([]any, int, error) {
	body, position, err := retainedPullBody(ctx, tx, project, previous, pull.Number)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	complete := any(int64(0))
	if err == nil && body[2] == pull.HeadSHA {
		complete = body[7]
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM review_pulls WHERE project_id=? AND snapshot_id=?", project, previous).Scan(&position); err != nil {
			return nil, 0, err
		}
		if position >= maxQueuePulls {
			return nil, 0, validationError("The native workspace queue is at its current capacity.")
		}
		body[8], body[9], body[10], body[11] = int64(0), int64(0), "[]", "[]"
	}
	body[0], body[1], body[2], body[3], body[4], body[5], body[6], body[7] = pull.Title, pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, complete
	return body, position, nil
}

func appendNativeRun(ctx context.Context, tx *sql.Tx, write *historySnapshotWrite, snapshot string, number int64, run RunInput) error {
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_run_versions WHERE project_id=? AND source=? AND source_run_id=?)`, write.project, nativeSource, run.SourceRunID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return core.Fail("observation_conflict", "A native captured run already exists; append decisions without replacing source evidence.", 409)
	}
	var order int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(run_order),-1)+1 FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=?`, write.project, snapshot, number).Scan(&order); err != nil {
		return err
	}
	return write.run(ctx, tx, nativeSource, number, order, run)
}

func publishNativeAttempt(ctx context.Context, tx *sql.Tx, write *historySnapshotWrite, previousSnapshot, stagedSnapshot string, number int64, a Attempt) error {
	if a.SourceRunID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? AND source=? AND source_run_id=? AND channel=? AND head_sha=?`, write.project, stagedSnapshot, number, nativeSource, a.SourceRunID, a.Channel, a.HeadSHA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return validationError("A native attempt must link to its own captured run on this PR.")
		}
	}
	var position int
	// No attempt has been staged yet. Resolve an existing attempt's position
	// from the prior workspace snapshot, rather than traversing retained intervals
	// at the new snapshot revision. For a new attempt, use the indexed current
	// placement table directly; it still reflects the old workspace at this point.
	err := tx.QueryRowContext(ctx, `SELECT position FROM review_attempts WHERE project_id=? AND snapshot_id=? AND source=? AND source_attempt_id=?`, write.project, previousSnapshot, nativeSource, a.SourceAttemptID).Scan(&position)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_attempts WHERE project_id=? AND pull_number=?`, write.project, number).Scan(&position); err != nil {
			return err
		}
	}
	return write.attempt(ctx, tx, nativeSource, number, position, a)
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

func rebuildNativeSummary(ctx context.Context, tx *sql.Tx, write *historySnapshotWrite, snapshot string, number int64, position int, body []any) error {
	if err := validateCurrentSummaryFront(ctx, tx, write.project, number); err != nil {
		return err
	}
	runs, err := write.recentCurrentRuns(ctx, tx, number)
	if err != nil {
		return err
	}
	attempts, err := write.latestCurrentAttempts(ctx, tx, number)
	if err != nil {
		return err
	}
	totals, err := write.prospectiveTotals(ctx, tx, number)
	if err != nil {
		return err
	}
	if totals.incomplete != 0 {
		body[7] = int64(0)
	}
	body[8], body[9], body[10], body[11] = totals.runs, totals.attempts, mustJSON(runs), mustJSON(attempts)
	return write.pull(ctx, tx, number, position, body)
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
