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

// runReads coalesces source identities only within one bounded selection phase.
type runReads struct {
	reader TargetReader
	repos  map[int64]reviewprovider.Repository
	pulls  map[string]map[int64]reviewprovider.Pull
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

type runChoice struct {
	state, reason string
	sourceReason  string
	prefix        []SourcePlan
	number        int64
	head          string
}

func runReason(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) {
		if failure.Code == "review_held" {
			if details, ok := failure.Details.(map[string]string); ok && identifier.MatchString(details["reason"]) {
				return details["reason"]
			}
		}
	}
	return "source_unavailable"
}

func (e *Engine) chooseRun(ctx context.Context, r Run, repo int64, c Configuration, reader TargetReader) (runChoice, error) {
	reads := &runReads{reader: reader, repos: map[int64]reviewprovider.Repository{}, pulls: map[string]map[int64]reviewprovider.Pull{}}
	repository, err := reads.Repository(ctx, repo)
	if err != nil {
		return runChoice{state: "held", reason: runReason(err), sourceReason: runReason(err)}, nil
	}
	if repository.ID != repo || !repositoryPattern(repository.FullName) || repository.DefaultBranch == "" {
		return runChoice{state: "held", reason: "repository_changed", sourceReason: "repository_changed"}, nil
	}
	choice := runChoice{state: "empty", reason: "queue_complete"}
	for _, n := range c.Queue {
		if ctx.Err() != nil {
			return runChoice{state: "held", reason: "source_unavailable", sourceReason: "source_unavailable"}, nil
		}
		pull, err := reads.Pull(ctx, repository.FullName, n)
		if err != nil {
			return runChoice{state: "held", reason: runReason(err), sourceReason: runReason(err)}, nil
		}
		if !sameTarget(pull, repo, n) || pull.State != "open" && pull.State != "closed" || pull.Merged && pull.State != "closed" {
			return runChoice{state: "held", reason: "source_unavailable", sourceReason: "source_unavailable"}, nil
		}
		plan := SourcePlan{Repository: repository, Pull: pull, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
		if !pull.Merged && pull.State == "open" {
			plan, err = ResolveSource(ctx, reads, repo, c, Request{PullNumber: n, HeadSHA: pull.HeadSHA, Channel: r.Channel})
			if err != nil {
				return runChoice{state: "held", reason: runReason(err), sourceReason: runReason(err)}, nil
			}
		}
		choice.prefix = append(choice.prefix, plan)
		var target int64
		var reason string
		err = e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			target, reason, err = e.runFront(ctx, tx, r, repo, []SourcePlan{plan})
			return err
		})
		if err != nil {
			return runChoice{}, err
		}
		if reason != "" {
			choice.state, choice.reason = "held", reason
			return choice, nil
		}
		if target != 0 {
			if r.ExpectedPullNumber != 0 && r.ExpectedPullNumber != target {
				choice.state, choice.reason = "held", "expected_pr_mismatch"
				return choice, nil
			}
			choice.state, choice.reason, choice.number, choice.head = "selected", "", target, pull.HeadSHA
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
					worked, err := e.runNext(workerCtx, owned)
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
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT project_id,id,revision FROM review_selections WHERE state='queued' ORDER BY created_at,id LIMIT 1`).Scan(&project, &id, &expected)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || id == "" {
		return false, err
	}
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
	if err == nil && changed != "" {
		e.notifyOperation(changed)
	}
	if err != nil || selected == nil {
		return changed != "", err
	}
	r := *selected
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	choice, readErr := e.chooseRun(readCtx, r, repo, configuration, readers[configuration.CapabilityID])
	if readCtx.Err() != nil {
		choice = runChoice{state: "held", reason: "source_unavailable", sourceReason: "source_unavailable"}
		readErr = nil
	}
	cancel()
	if ctx.Err() != nil {
		return true, nil
	}
	if readErr != nil {
		return true, readErr
	}
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		if err = publishSourcePlans(ctx, tx, core.Scope{ProjectID: current.ProjectID, PrincipalID: current.ActorID, WorkerID: current.WorkerID}, repo, metadataPins, choice.prefix); err != nil {
			return err
		}
		if err = writeFrontFrame(ctx, tx, current, repo, c, choice); err != nil {
			return err
		}
		if choice.state == "held" {
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
			return updateRun(ctx, tx, current, "held", failure.Code, "")
		}
		if _, err = tx.ExecContext(ctx, `RELEASE review_selection_child`); err != nil {
			return err
		}
		return updateRun(ctx, tx, current, "selected", "", receipt.ID)
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
