package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"reflect"
	"slices"
	"sync"
	"time"
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
	prefix        []SourcePlan
	pausedSuffix  []int64
	number        int64
	head          string
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
		if !errors.As(err, &failure) || failure.Code != "overloaded" {
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

func (e *Engine) chooseRun(ctx context.Context, r Run, repo int64, c Configuration, reader TargetReader) (runChoice, error) {
	return e.chooseRunWithSourceBudget(ctx, r, repo, c, reader, 15*time.Second)
}

func (e *Engine) chooseRunWithSourceBudget(ctx context.Context, r Run, repo int64, c Configuration, reader TargetReader, sourceBudget time.Duration) (runChoice, error) {
	if len(c.Queue) == 0 {
		return runChoice{state: "empty", reason: "queue_complete"}, nil
	}
	reads := &runReads{reader: reader, repos: map[int64]reviewprovider.Repository{}, pulls: map[string]map[int64]reviewprovider.Pull{}}
	source := runSourceBudget{remaining: sourceBudget}
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
	err = retryRunSelectionLocal(ctx, func() error {
		return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			history, err = readRunHistory(ctx, tx, r.ProjectID, repo, queue)
			return err
		})
	})
	if err != nil {
		return runChoice{}, err
	}
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
			err = retryRunSelectionLocal(ctx, func() error {
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
		pausedSuffix := false
		err = retryRunSelectionLocal(ctx, func() error {
			target, reason = 0, ""
			bindingHeld = false
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
				target, reason, err = e.runFront(ctx, tx, current, repo, choice.prefix)
				if err == nil && reason != "" && !globalFrontHold(reason) {
					pausedSuffix, err = pausedRunSuffix(ctx, tx, r.ProjectID, choice.prefix, queue[i+1:])
				}
				return err
			})
		})
		if err != nil {
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
			if bindingHeld || globalFrontHold(reason) {
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
	if len(owned) == 0 {
		return nil, e.RecoverReviewRuns(ctx)
	}
	if !e.runRunning.CompareAndSwap(false, true) {
		return nil, conflict()
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
			err := tx.QueryRowContext(ctx, `SELECT s.project_id,s.id,s.revision FROM review_selections s
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
	if err == nil && changed != "" {
		e.notifyOperation(changed)
	}
	if err != nil || selected == nil {
		return changed != "", err
	}
	r := *selected
	choice, readErr := e.chooseRun(ctx, r, repo, configuration, readers[configuration.CapabilityID])
	if ctx.Err() != nil {
		return true, nil
	}
	if readErr != nil {
		return true, readErr
	}
	return e.finishRunChoice(ctx, r, repo, metadataPins, choice)
}

// finishRunChoice validates and records a source selection in one local write.
func (e *Engine) finishRunChoice(ctx context.Context, r Run, repo int64, metadataPins map[int64]reviews.SourceMetadataPin, choice runChoice) (bool, error) {
	err := retryRunSelectionLocal(ctx, func() error {
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) (resultErr error) {
			defer func() {
				if resultErr == nil {
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
			}()
			current, err := readRun(ctx, tx, r.ProjectID, r.ID)
			if err != nil {
				return err
			}
			if current.State != r.State || current.Revision != r.Revision {
				return conflict()
			}
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
				if choice.sourceReason == "" && !globalFrontHold(choice.reason) && choice.reason != "expected_pr_mismatch" && choice.reason != "source_prefix_incomplete" {
					// Local readiness may have changed after chooseRun's read,
					// even for a later PR masked by the first held reason. Do not
					// settle that newer eligible basis behind a
					// stale hold. A fresh ordinary selection must reacquire source
					// facts rather than dispatch directly from this recheck.
					_, reason, err := e.runFront(ctx, tx, current, repo, choice.prefix)
					if err != nil {
						return err
					}
					if reason != choice.reason {
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
