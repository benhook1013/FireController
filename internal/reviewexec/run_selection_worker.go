package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// runSourceBudget bounds cumulative source acquisition, excluding local SQL
// admission and its retry backoff. It lives only for one selection phase.
type runSourceBudget struct{ remaining time.Duration }

func (b *runSourceBudget) read(ctx context.Context, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.remaining <= 0 {
		return errRunSourceBudget
	}
	started := time.Now()
	sourceCtx, cancel := context.WithTimeout(ctx, b.remaining)
	defer cancel()
	err := read(sourceCtx)
	b.remaining -= time.Since(started)
	// A reader that returns success after its deadline still has no usable
	// source result. Parent cancellation wins; check before canceling our own
	// context, including elapsed exhaustion before timer delivery catches up.
	if err := ctx.Err(); err != nil {
		return err
	}
	if sourceCtx.Err() != nil {
		if errors.Is(sourceCtx.Err(), context.DeadlineExceeded) {
			return errRunSourceBudget
		}
		return sourceCtx.Err()
	}
	if b.remaining <= 0 {
		return errRunSourceBudget
	}
	return err
}

// runReads coalesces source identities only within one bounded selection phase.
type runReads struct {
	reader  TargetReader
	repos   map[int64]reviewprovider.Repository
	pulls   map[string]map[int64]reviewprovider.Pull
	parents map[string]reviewprovider.Pull
}

func (r *runReads) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if v, ok := r.repos[id]; ok {
		return v, nil
	}
	v, err := r.reader.Repository(ctx, id)
	if err == nil {
		r.repos[id] = v
	}
	return v, err
}
func (r *runReads) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	if v, ok := r.pulls[repo][n]; ok {
		return v, nil
	}
	v, err := r.reader.Pull(ctx, repo, n)
	if err == nil {
		if r.pulls[repo] == nil {
			r.pulls[repo] = map[int64]reviewprovider.Pull{}
		}
		r.pulls[repo][n] = v
	}
	return v, err
}

type runPullResult struct {
	pull reviewprovider.Pull
	err  error
}

// At most four raw metadata GETs are in flight per selection (eight across its
// two workers). Results remain ordered; only joined successes enter phase-local
// caches. Lookahead never supplies a selection or publication by itself.
func (r *runReads) pullWindow(ctx context.Context, repo string, repositoryID int64, numbers []int64) []runPullResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]runPullResult, len(numbers))
	completed := make([]chan struct{}, len(numbers))
	var workers sync.WaitGroup
	for i, number := range numbers {
		completed[i] = make(chan struct{})
		if value, ok := r.pulls[repo][number]; ok {
			results[i].pull = value
			close(completed[i])
			continue
		}
		workers.Go(func() {
			defer close(completed[i])
			results[i].pull, results[i].err = r.reader.Pull(ctx, repo, number)
		})
	}
	consumed := len(numbers)
	for i, number := range numbers {
		select {
		case <-ctx.Done():
			consumed = i
		case <-completed[i]:
			result := results[i]
			if result.err == nil && sameTarget(result.pull, repositoryID, number) && result.pull.Merged && result.pull.State == "closed" {
				continue
			}
			// A live/closed-unmerged candidate (or ordered error) needs local
			// evaluation before any farther source work can matter. Cancel
			// discarded lookahead, including a slow reader, then join it.
			consumed = i + 1
		}
		cancel()
		break
	}
	workers.Wait()
	for i, result := range results {
		// A successfully joined lookahead read is reusable only when the
		// ordered walk later reaches that identity and checks local authority.
		if result.err == nil && sameTarget(result.pull, repositoryID, numbers[i]) {
			if r.pulls[repo] == nil {
				r.pulls[repo] = map[int64]reviewprovider.Pull{}
			}
			r.pulls[repo][numbers[i]] = result.pull
		}
	}
	return results[:consumed]
}

func (r *runReads) ParentPull(ctx context.Context, repository, branch string) (reviewprovider.Pull, error) {
	key := repository + "\x00" + branch
	if value, ok := r.parents[key]; ok {
		return value, nil
	}
	reader, ok := r.reader.(ParentReader)
	if !ok {
		return reviewprovider.Pull{}, sourceHold("parent_unavailable")
	}
	value, err := reader.ParentPull(ctx, repository, branch)
	if err == nil {
		if r.parents == nil {
			r.parents = map[string]reviewprovider.Pull{}
		}
		r.parents[key] = value
		if value.Number > 0 {
			if r.pulls[repository] == nil {
				r.pulls[repository] = map[int64]reviewprovider.Pull{}
			}
			r.pulls[repository][value.Number] = value
		}
	}
	return value, err
}

type runChoice struct {
	state, reason string
	sourceReason  string
	sourceElapsed time.Duration
	prefix        []SourcePlan
	pausedSuffix  []int64
	number        int64
	head          string
	expectedHeld  bool
}

// retryRunSelectionLocal retains completed source reads while only retrying
// transient local admission. Each transaction keeps its own bounded context.
func retryRunSelectionLocal(ctx context.Context, local func() error) error {
	delay := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := local()
		var failure *core.Error
		if automaticProjectRefused(err) || !errors.As(err, &failure) || failure.Code != "overloaded" {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}

// retryRunSelectionRead preserves brief contention recovery without letting a
// repeatedly expired prefix read retain a selection worker indefinitely.
func retryRunSelectionRead(ctx context.Context, local func() error) error {
	attempts := 0
	return retryRunSelectionLocal(ctx, func() error {
		err := local()
		attempts++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var failure *core.Error
		if attempts >= 3 && errors.As(err, &failure) && failure.Code == "overloaded" {
			return core.Fail("local_read_unavailable", "Local selection verification remained unavailable after bounded retries.", 503)
		}
		return err
	})
}

func (e *Engine) chooseRun(ctx context.Context, r Run, repo int64, c Configuration, reader TargetReader) (runChoice, error) {
	return e.chooseRunWithSourceBudget(ctx, r, repo, c, reader, e.runSourceBudget)
}

func (e *Engine) chooseRunWithSourceBudget(ctx context.Context, r Run, repo int64, c Configuration, reader TargetReader, sourceBudget time.Duration) (result runChoice, resultErr error) {
	if len(c.Queue) == 0 {
		return runChoice{state: "empty", reason: "queue_complete"}, nil
	}
	reads := &runReads{reader: reader, repos: map[int64]reviewprovider.Repository{}, pulls: map[string]map[int64]reviewprovider.Pull{}}
	source := runSourceBudget{remaining: sourceBudget}
	defer func() {
		result.sourceElapsed = sourceBudget - source.remaining
	}()
	var repository reviewprovider.Repository
	err := source.read(ctx, func(ctx context.Context) error {
		var err error
		repository, err = reads.Repository(ctx, repo)
		return err
	})
	if err != nil {
		return runChoice{state: "held", reason: runReason(err), sourceReason: runReason(err)}, nil
	}
	if repository.ID != repo || !repositoryPattern(repository.FullName) || repository.DefaultBranch == "" {
		return runChoice{state: "held", reason: "repository_changed", sourceReason: "repository_changed"}, nil
	}
	choice := runChoice{state: "empty", reason: "queue_complete"}
	sourceFailure := func(reason string) runChoice {
		choice.state, choice.reason, choice.sourceReason = "held", reason, reason
		return choice
	}
	queue := c.Queue
	if index := slices.Index(queue, r.ExpectedPullNumber); index >= 0 {
		queue = queue[:index+1]
	}
	var history map[int64]SourcePlan
	err = retryRunSelectionRead(ctx, func() error {
		return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			history, err = readRunHistory(ctx, tx, r.ProjectID, repo, queue)
			return err
		})
	})
	if err != nil {
		return runChoice{}, err
	}
	windows := map[int64]runPullResult{}
	for i, n := range queue {
		if err := ctx.Err(); err != nil {
			return runChoice{}, err
		}
		plan, retained := history[n]
		neededParent := false
		for _, previous := range choice.prefix {
			if previous.ParentPullNumber == n {
				neededParent = true
				break
			}
		}
		if !retained || neededParent {
			var paused bool
			err = retryRunSelectionRead(ctx, func() error {
				return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
					control, err := readPullScheduling(ctx, tx, r.ProjectID, n)
					paused = control.Paused
					return err
				})
			})
			if err != nil {
				return runChoice{}, err
			}
			if paused && !neededParent {
				plan = SourcePlan{Repository: repository, Pull: reviewprovider.Pull{RepositoryID: repo, Number: n}, SourceReason: "pull_scheduling_paused", schedulingPaused: true}
			} else {
				if _, fetched := windows[n]; !fetched {
					// History and paused controls are barriers, never permission
					// for speculative reads. Future control damage only stops
					// lookahead; the ordered member retains its normal checks.
					numbers := []int64{n}
					window := min(i+4, len(queue))
					if len(queue) <= 2 {
						window = i + 1
					}
					for j := i + 1; j < window; j++ {
						next := queue[j]
						if _, retained := history[next]; retained {
							break
						}
						var blocked bool
						controlErr := retryRunSelectionRead(ctx, func() error {
							return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
								control, err := readPullScheduling(ctx, tx, r.ProjectID, next)
								blocked = control.Paused
								return err
							})
						})
						if controlErr != nil || blocked {
							break
						}
						numbers = append(numbers, next)
					}
					err = source.read(ctx, func(ctx context.Context) error {
						for j, result := range reads.pullWindow(ctx, repository.FullName, repo, numbers) {
							windows[numbers[j]] = result
						}
						return nil
					})
					if err != nil {
						return sourceFailure(runReason(err)), nil
					}
				}
				if result, fetched := windows[n]; fetched && result.err != nil {
					return sourceFailure(runReason(result.err)), nil
				}
				plan, err = readRunPlan(ctx, &source, reads, repo, c, r.Channel, n)
				if err != nil {
					return sourceFailure(runReason(err)), nil
				}
			}
		}
		choice.prefix = append(choice.prefix, plan)
		if err = e.resolveRunParents(ctx, &source, reads, repo, c, r.Channel, choice.prefix); err != nil {
			return sourceFailure(runReason(err)), nil
		}
		var target int64
		var reason string
		bindingHeld := false
		expectedHeld := false
		pausedSuffix := false
		err = retryRunSelectionRead(ctx, func() error {
			target, reason = 0, ""
			bindingHeld = false
			expectedHeld = false
			pausedSuffix = false
			return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
				current, err := readRun(ctx, tx, r.ProjectID, r.ID)
				if err != nil {
					return err
				}
				if current.State != r.State || current.Revision != r.Revision {
					return conflict()
				}
				_, nowRepo, bindingReason, err := e.runBinding(ctx, tx, current)
				if err != nil {
					return err
				}
				if bindingReason == "" && nowRepo != repo {
					bindingReason = "repository_changed"
				}
				if bindingReason != "" {
					reason = bindingReason
					bindingHeld = true
					return nil
				}
				front, err := e.evaluateFront(ctx, tx, current, repo, choice.prefix, true)
				if err != nil {
					return err
				}
				if front.state == "ready" {
					target = front.plan.Pull.Number
				} else if front.state != "complete" {
					reason = front.reason
					expectedHeld = front.holdsPull(current.ExpectedPullNumber)
				}
				if reason != "" && !expectedHeld && !globalFrontHold(reason) {
					pausedSuffix, err = pausedRunSuffix(ctx, tx, r.ProjectID, choice.prefix, queue[i+1:])
					return err
				}
				return nil
			})
		})
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "local_read_unavailable" {
				choice.state, choice.reason = "held", "local_read_unavailable"
				return choice, nil
			}
			return runChoice{}, err
		}
		if reason != "" {
			choice.state, choice.reason = "held", reason
			if pausedSuffix {
				choice.pausedSuffix = slices.Clone(queue[i+1:])
				if r.ExpectedPullNumber != 0 {
					choice.reason = "expected_pr_mismatch"
				}
				return choice, nil
			}
			if bindingHeld || expectedHeld || globalFrontHold(reason) {
				choice.expectedHeld = expectedHeld
				return choice, nil
			}
			continue
		}
		choice.state, choice.reason = "empty", "queue_complete"
		if target != 0 {
			if r.ExpectedPullNumber != 0 && r.ExpectedPullNumber != target {
				choice.state, choice.reason = "held", "expected_pr_mismatch"
				return choice, nil
			}
			choice.state, choice.reason, choice.number = "selected", "", target
			for _, selected := range choice.prefix {
				if selected.Pull.Number == target {
					choice.head = selected.Pull.HeadSHA
					break
				}
			}
			return choice, nil
		}
	}
	if r.ExpectedPullNumber != 0 {
		choice.state, choice.reason = "held", "expected_pr_mismatch"
	}
	return choice, nil
}
func repositoryPattern(value string) bool { return repository.MatchString(value) }

func (e *Engine) StartReviewRuns(ctx context.Context, readers map[string]TargetReader) (<-chan error, error) {
	if ctx == nil {
		return nil, invalid()
	}
	owned := map[string]TargetReader{}
	for id, reader := range readers {
		if reader == nil || reflect.ValueOf(reader).Kind() == reflect.Pointer && reflect.ValueOf(reader).IsNil() {
			return nil, invalid()
		}
		if _, ok := e.capabilities[id]; !ok {
			return nil, invalid()
		}
		owned[id] = reader
	}
	if !e.runRunning.CompareAndSwap(false, true) {
		return nil, conflict()
	}
	if len(owned) == 0 {
		defer e.runRunning.Store(false)
		return nil, e.RecoverReviewRuns(ctx)
	}
	if err := e.RecoverReviewRuns(ctx); err != nil {
		e.runRunning.Store(false)
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		defer e.runRunning.Store(false)
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var wg sync.WaitGroup
		fail := make(chan error, 1)
		for range 2 {
			wg.Go(func() {
				timer := time.NewTimer(0)
				defer timer.Stop()
				for {
					select {
					case <-workerCtx.Done():
						return
					case <-e.runWake:
					case <-timer.C:
					}
					if workerCtx.Err() != nil {
						return
					}
					generated, err := e.coordinateAutomaticHosted(workerCtx, owned)
					var worked bool
					if err == nil {
						worked, err = e.runNext(workerCtx, owned)
					}
					worked = worked || generated
					if err != nil {
						if workerCtx.Err() != nil {
							return
						}
						select {
						case fail <- core.Fail("review_selection_persistence_failed", "Review selection stopped because local state could not be recorded; restart to recover pending reads.", 503):
						default:
						}
						cancel()
						return
					}
					if worked {
						timer.Reset(0)
					} else {
						timer.Reset(500 * time.Millisecond)
					}
				}
			})
		}
		wg.Wait()
		select {
		case err := <-fail:
			done <- err
		default:
			done <- nil
		}
		close(done)
	}()
	return done, nil
}
func (e *Engine) runNext(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	var selected *Run
	var configuration Configuration
	var metadataPins map[int64]reviews.SourceMetadataPin
	var repo int64
	var changed string
	var project, id string
	var expected int64
	err := retryRunSelectionLocal(ctx, func() error {
		project, id, expected = "", "", 0
		return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			// Request capacity is a scheduling constraint, not a terminal hold.
			// Skip saturated projects before source work so they cannot occupy
			// both selection workers or repeatedly reacquire provider facts.
			err := tx.QueryRowContext(ctx, `SELECT s.project_id,s.id,`+selectionClaimRevision+` FROM review_selections s
 WHERE s.state='queued'
 AND (SELECT count(*) FROM review_operations WHERE state IN ('queued','preparing','posting','active','uncertain'))<?
 AND (SELECT count(*) FROM review_operations o WHERE o.project_id=s.project_id AND o.state IN ('queued','preparing','posting','active','uncertain'))<?
 ORDER BY CASE WHEN s.channel='cli' AND (SELECT lower(repository) FROM projects WHERE id=s.project_id)='benhook1013/firemud' THEN 0 ELSE 1 END,s.created_at,s.id LIMIT 1`, requestGlobalCapacity, requestProjectCapacity).Scan(&project, &id, &expected)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		})
	})
	if err != nil || id == "" {
		return false, err
	}
	err = retryRunSelectionLocal(ctx, func() error {
		// A rolled-back claim cannot retain a selected pointer or notification.
		selected, changed = nil, ""
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			r, err := readRun(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if r.State == "queued" && expected == 0 {
				return runUnavailable()
			}
			if r.State != "queued" || r.Revision != expected {
				return nil
			}
			var reason string
			configuration, repo, reason, err = e.runBinding(ctx, tx, r)
			if err != nil {
				return err
			}
			if reason == "" && readers[configuration.CapabilityID] == nil {
				reason = "source_reader_unavailable"
			}
			changed = r.ID
			if reason != "" {
				return updateRun(ctx, tx, r, "held", reason, "")
			}
			if e.publishAttempt != nil {
				metadataPins, err = reviews.PinSourceMetadata(ctx, tx, r.ProjectID, configuration.Queue)
				if err != nil {
					return err
				}
			}
			if err = updateRun(ctx, tx, r, "selecting", "", ""); err != nil {
				return err
			}
			r.State = "selecting"
			r.Revision++
			selected = &r
			return nil
		})
	})
	if automaticProjectRefused(err) {
		return e.holdRunLocalRefusal(ctx, Run{ProjectID: project, ID: id, Revision: expected, State: "queued"})
	}
	if err == nil && changed != "" {
		e.notifyOperation(changed)
	}
	if err != nil || selected == nil {
		return changed != "", err
	}
	r := *selected
	started := time.Now()
	choice, readErr := e.chooseRun(ctx, r, repo, configuration, readers[configuration.CapabilityID])
	selectionElapsed := time.Since(started)
	defer func() {
		logSlowRunSelection(ctx, r, choice, len(configuration.Queue), selectionElapsed, time.Since(started)-selectionElapsed)
	}()
	if ctx.Err() != nil {
		return true, nil
	}
	var readFailure *core.Error
	if automaticProjectRefused(readErr) || errors.As(readErr, &readFailure) && readFailure.Code == "local_read_unavailable" {
		// Earlier history/pause reads have no complete source prefix to publish.
		// Retain the exact selecting revision as a local hold, not a worker error.
		return e.holdRunLocalRefusal(ctx, r)
	}
	if readErr != nil {
		return true, readErr
	}
	worked, err := e.finishRunChoice(ctx, r, repo, metadataPins, choice)
	if automaticProjectRefused(err) {
		return e.holdRunLocalRefusal(ctx, r)
	}
	return worked, err
}

func logSlowRunSelection(ctx context.Context, r Run, choice runChoice, queueSize int, selectionElapsed, settlementElapsed time.Duration) {
	total := selectionElapsed + settlementElapsed
	if total < 10*time.Second {
		return
	}
	slog.WarnContext(ctx, "Review selection was slow",
		"project_id", r.ProjectID, "selection_id", r.ID, "channel", r.Channel,
		"queue_size", queueSize, "evaluated_prefix", len(choice.prefix),
		"duration_ms", total.Milliseconds(), "source_read_ms", choice.sourceElapsed.Milliseconds(),
		"local_selection_ms", (selectionElapsed - choice.sourceElapsed).Milliseconds(),
		"settlement_ms", settlementElapsed.Milliseconds())
}

// A validated retained-data refusal belongs to this selection, not the worker
// pool. Persist only the hold after the failed claim/finish transaction rolled
// back. Explicit requests retain local-read attribution, never automatic intent.
func (e *Engine) holdRunLocalRefusal(ctx context.Context, expected Run) (bool, error) {
	changed := false
	err := retryRunSelectionLocal(ctx, func() error {
		changed = false
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readRun(ctx, tx, expected.ProjectID, expected.ID)
			if automaticProjectRefused(err) || err == nil && current.Revision == math.MaxInt64 {
				claim := expected
				if claim.Revision < 1 || claim.Revision == math.MaxInt64 {
					claim.Revision = 0
				}
				changed, err = quarantineSelection(ctx, tx, claim)
				return err
			}
			if err != nil {
				return err
			}
			if current.State != expected.State || current.Revision != expected.Revision {
				return nil
			}
			var automatic bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id=?)`, current.ProjectID, current.ID).Scan(&automatic); err != nil {
				return err
			}
			reason := "local_read_unavailable"
			if automatic {
				reason = "automatic_scheduling_unavailable"
			}
			if err = updateRun(ctx, tx, current, "held", reason, ""); err != nil {
				return err
			}
			changed = true
			return nil
		})
	})
	if err == nil && changed {
		e.notifyOperation(expected.ID)
	}
	return changed, err
}

// finishRunChoice validates and records a source selection in one local write.
func (e *Engine) finishRunChoice(ctx context.Context, r Run, repo int64, metadataPins map[int64]reviews.SourceMetadataPin, choice runChoice) (bool, error) {
	err := retryRunSelectionLocal(ctx, func() error {
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) (resultErr error) {
			current, err := readRun(ctx, tx, r.ProjectID, r.ID)
			if err != nil {
				return err
			}
			if current.State != r.State || current.Revision != r.Revision {
				return conflict()
			}
			if _, err = tx.ExecContext(ctx, `SAVEPOINT review_selection_finish`); err != nil {
				return err
			}
			bindingValidated := false
			defer func() {
				if resultErr == nil && bindingValidated {
					var pull int64
					var reason string
					if err := tx.QueryRowContext(ctx, `SELECT COALESCE(o.pull_number,0),s.reason FROM review_selections s LEFT JOIN review_operations o ON o.id=s.operation_id WHERE s.id=? AND s.project_id=?`, r.ID, r.ProjectID).Scan(&pull, &reason); err != nil {
						resultErr = err
						return
					}
					if reason != "selection_changed" {
						resultErr = e.settleAutomaticBasis(ctx, tx, r, pull)
					}
				}
				if automaticProjectRefused(resultErr) {
					var automatic bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE selection_id=? AND project_id=?)`, r.ID, r.ProjectID).Scan(&automatic); err != nil {
						resultErr = err
						return
					}
					if !automatic {
						return // Explicit selections retain their original error and rollback.
					}
					// Settlement is part of atomic selection. Discard any source,
					// child and event writes before retaining a local hold. The
					// accepted basis stays intact for tick/restart suppression.
					if _, err := tx.ExecContext(ctx, `ROLLBACK TO review_selection_finish`); err != nil {
						resultErr = err
						return
					}
					resultErr = updateRun(ctx, tx, current, "held", "automatic_scheduling_unavailable", "")
				}
				if resultErr == nil {
					_, resultErr = tx.ExecContext(ctx, `RELEASE review_selection_finish`)
				}
			}()
			c, nowRepo, reason, err := e.runBinding(ctx, tx, current)
			if err != nil {
				return err
			}
			if reason == "" && nowRepo != repo {
				reason = "repository_changed"
			}
			if reason != "" {
				return updateRun(ctx, tx, current, "held", reason, "")
			}
			bindingValidated = true
			validHistory, err := runHistoryCurrent(ctx, tx, current, repo, c, choice)
			if err != nil {
				return err
			}
			if !validHistory {
				return updateRun(ctx, tx, current, "held", "selection_changed", "")
			}
			if len(choice.pausedSuffix) > 0 {
				paused, err := pausedRunSuffix(ctx, tx, current.ProjectID, choice.prefix, choice.pausedSuffix)
				if err != nil {
					return err
				}
				if !paused {
					return updateRun(ctx, tx, current, "held", "selection_changed", "")
				}
			}
			if err = publishSourcePlans(ctx, tx, core.Scope{ProjectID: current.ProjectID, PrincipalID: current.ActorID, WorkerID: current.WorkerID}, repo, metadataPins, choice.prefix); err != nil {
				return err
			}
			if err = writeFrontFrame(ctx, tx, current, repo, c, choice); err != nil {
				return err
			}
			if choice.state == "held" {
				if choice.sourceReason == "" && !globalFrontHold(choice.reason) && choice.reason != "expected_pr_mismatch" && choice.reason != "source_prefix_incomplete" && choice.reason != "local_read_unavailable" {
					// Local readiness may have changed after chooseRun's read,
					// even for a later PR masked by the first held reason. Do not
					// settle that newer eligible basis behind a
					// stale hold. A fresh ordinary selection must reacquire source
					// facts rather than dispatch directly from this recheck.
					front, err := e.evaluateFront(ctx, tx, current, repo, choice.prefix, true)
					if err != nil {
						return err
					}
					if front.reason != choice.reason || choice.expectedHeld && !front.holdsPull(current.ExpectedPullNumber) {
						return updateRun(ctx, tx, current, "held", "selection_changed", "")
					}
				}
				return updateRun(ctx, tx, current, "held", choice.reason, "")
			}
			front, reason, err := e.runFront(ctx, tx, current, repo, choice.prefix)
			if err != nil {
				return err
			}
			if reason != "" {
				return updateRun(ctx, tx, current, "held", reason, "")
			}
			if front != choice.number {
				return updateRun(ctx, tx, current, "held", "selection_changed", "")
			}
			if choice.state == "empty" {
				return updateRun(ctx, tx, current, "empty", "queue_complete", "")
			}
			if !slices.Contains(c.Queue, choice.number) {
				return updateRun(ctx, tx, current, "held", "configuration_changed", "")
			}
			// This scope is trusted attribution only, following the current checks above.
			attribution := core.Scope{ProjectID: current.ProjectID, PrincipalID: current.ActorID, WorkerID: current.WorkerID}
			if _, err = tx.ExecContext(ctx, `SAVEPOINT review_selection_child`); err != nil {
				return err
			}
			receipt, err := e.enqueueRequestTx(ctx, tx, attribution, Request{ExpectedConfigurationRevision: current.ConfigurationRevision, PullNumber: choice.number, HeadSHA: choice.head, Channel: current.Channel})
			if err != nil {
				var failure *core.Error
				if !errors.As(err, &failure) || (failure.Status != 409 && failure.Code != "overloaded") {
					return err
				}
				if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO review_selection_child`); rollbackErr != nil {
					return rollbackErr
				}
				if _, releaseErr := tx.ExecContext(ctx, `RELEASE review_selection_child`); releaseErr != nil {
					return releaseErr
				}
				if failure.Code == "overloaded" {
					// Capacity filled after candidate admission. Yield this worker;
					// the next eligible pass rechecks binding and acquires fresh
					// source facts before attempting the same selection's child.
					return updateRun(ctx, tx, current, "queued", "", "")
				}
				return updateRun(ctx, tx, current, "held", failure.Code, "")
			}
			if _, err = tx.ExecContext(ctx, `RELEASE review_selection_child`); err != nil {
				return err
			}
			var automatic bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id=?)`, current.ProjectID, current.ID).Scan(&automatic); err != nil {
				return err
			}
			if !automatic {
				if err := e.followExplicitWork(ctx, tx, attribution, choice.number); err != nil {
					return err
				}
			}
			return updateRun(ctx, tx, current, "selected", "", receipt.ID)
		})
	})
	if err == nil {
		e.notifyOperation(r.ID)
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	return true, err
}

// Only comparison-local refusals can be skipped for another independent PR.
// Transport, account and configuration failures retain the global source hold.
func perPullSourceHold(reason string) bool {
	switch reason {
	case "pull_draft", "parent_moved", "parent_unavailable", "parent_identity_changed", "default_base_changed", "target_changed":
		return true
	}
	return false
}
