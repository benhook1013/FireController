package main

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// One operator-owned reader supplies both source reads and publication I/O.
// Idle lifecycle checks must never call any of these provider methods.
type checkpointWorkerProvider struct {
	sourceWorkerReader
	checkpointCalls atomic.Int32
}

func (p *checkpointWorkerProvider) CurrentActor(context.Context) (reviewprovider.Author, error) {
	p.checkpointCalls.Add(1)
	return reviewprovider.Author{}, errors.New("unexpected idle checkpoint actor read")
}
func (p *checkpointWorkerProvider) PostIssueComment(context.Context, string, int64, string) (reviewprovider.IssueComment, error) {
	p.checkpointCalls.Add(1)
	return reviewprovider.IssueComment{}, errors.New("unexpected idle checkpoint post")
}
func (p *checkpointWorkerProvider) IssueComments(context.Context, string, int64) ([]reviewprovider.IssueComment, error) {
	p.checkpointCalls.Add(1)
	return nil, errors.New("unexpected idle checkpoint history read")
}

func TestCheckpointWorkersShareSelectedReaderAndReleaseAllOwnership(t *testing.T) {
	_, engine := sourceWorkersFixture(t)
	provider := &checkpointWorkerProvider{}
	driver := &sourceBoundDriver{reader: provider}
	drivers := map[string]reviewexec.Driver{"source-capability": driver}
	providers := map[string]reviewexec.CheckpointProvider{"source-capability": provider}
	for round := int32(1); round <= 2; round++ {
		ctx, cancel := context.WithCancel(t.Context())
		done, err := startReviewWorkers(ctx, engine, drivers)
		if err != nil || done == nil {
			cancel()
			t.Fatal("checkpoint aggregate startup", err)
		}
		duplicate, err := engine.StartCheckpointPublications(t.Context(), providers)
		if err == nil || duplicate != nil {
			cancel()
			waitSourceWorkers(t, done)
			t.Fatal("daemon did not own checkpoint worker", err)
		}
		cancel()
		waitSourceWorkers(t, done)
		if driver.selections.Load() != round || provider.calls.Load() != 0 || provider.checkpointCalls.Load() != 0 {
			t.Fatal("reader selected multiple times or idle provider work", driver.selections.Load(), provider.calls.Load(), provider.checkpointCalls.Load())
		}
	}
	// A completed aggregate releases checkpoint ownership as well as supervision
	// and source ownership (the repeated aggregate above exercises both).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := engine.StartCheckpointPublications(ctx, providers)
	if err != nil || done == nil {
		t.Fatal("checkpoint owner outlived aggregate", err)
	}
	cancel()
	waitSourceWorkers(t, done)
}

func TestCheckpointWorkersStartupFailureCancelsAndJoinsStartedSiblings(t *testing.T) {
	_, engine := sourceWorkersFixture(t)
	provider := &checkpointWorkerProvider{}
	ownerCtx, ownerCancel := context.WithCancel(t.Context())
	defer ownerCancel()
	separate, err := engine.StartCheckpointPublications(ownerCtx, map[string]reviewexec.CheckpointProvider{"source-capability": provider})
	if err != nil || separate == nil {
		t.Fatal("separate checkpoint ownership", err)
	}
	defer func() { ownerCancel(); waitSourceWorkers(t, separate) }()
	driver := &sourceStartupObserver{sourceBoundDriver: sourceBoundDriver{reader: provider}, canceled: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": driver})
	if err == nil || done == nil {
		close(driver.release)
		t.Fatal("checkpoint duplicate startup lost owned failure", err)
	}
	select {
	case <-driver.canceled:
	case <-time.After(3 * time.Second):
		close(driver.release)
		t.Fatal("checkpoint startup failure did not cancel sibling")
	}
	select {
	case <-done:
		close(driver.release)
		t.Fatal("startup aggregate returned before owned observer joined")
	default:
	}
	close(driver.release)
	waitSourceWorkers(t, done)
	if driver.selections.Load() != 1 || provider.calls.Load() != 0 || provider.checkpointCalls.Load() != 0 {
		t.Fatal("startup repeated selection or contacted provider")
	}
	// Failed aggregate has released the source worker; the unrelated preexisting
	// checkpoint owner remains live until its own owner cancels it.
	sourceCtx, sourceCancel := context.WithCancel(t.Context())
	defer sourceCancel()
	sourceDone, sourceErr := engine.StartSourceRefresh(sourceCtx, map[string]reviewexec.TargetReader{"source-capability": provider})
	if sourceErr != nil || sourceDone == nil {
		t.Fatal("startup failure leaked source ownership", sourceErr)
	}
	sourceCancel()
	waitSourceWorkers(t, sourceDone)
	select {
	case <-separate:
		t.Fatal("failed aggregate canceled unrelated checkpoint owner")
	default:
	}
}

func TestCheckpointWorkersRuntimeFailureCancelsAndJoinsStartedSiblings(t *testing.T) {
	store, engine := sourceWorkersFixture(t)
	provider := &checkpointWorkerProvider{}
	driver := &sourceStartupObserver{sourceBoundDriver: sourceBoundDriver{reader: provider}, canceled: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": driver})
	if err != nil || done == nil {
		close(driver.release)
		t.Fatal("worker startup", err)
	}
	err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE review_checkpoint_publications`)
		return err
	})
	if err != nil {
		close(driver.release)
		t.Fatal(err)
	}
	select {
	case <-driver.canceled:
	case <-time.After(3 * time.Second):
		close(driver.release)
		t.Fatal("checkpoint queue storage failure did not cancel sibling")
	}
	select {
	case <-done:
		close(driver.release)
		t.Fatal("runtime aggregate returned before observer joined")
	default:
	}
	close(driver.release)
	select {
	case err := <-done:
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "checkpoint_publication_persistence_failed" {
			t.Fatal("checkpoint failure lost safe classification", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("checkpoint failure did not join workers")
	}
	if driver.selections.Load() != 1 || provider.calls.Load() != 0 || provider.checkpointCalls.Load() != 0 {
		t.Fatal("runtime failure selected extra readers/contacted provider")
	}
}

func TestCheckpointWorkersDriverlessAndSourceOnlyDoNotAcquirePublicationLoop(t *testing.T) {
	for _, mode := range []string{"disabled", "source-only"} {
		t.Run(mode, func(t *testing.T) {
			_, engine := sourceWorkersFixture(t)
			driver := &sourceBoundDriver{reader: &sourceWorkerReader{}}
			drivers := map[string]reviewexec.Driver{}
			if mode == "source-only" {
				drivers["source-capability"] = driver
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, err := startReviewWorkers(ctx, engine, drivers)
			if err != nil || mode == "disabled" && done != nil || mode == "source-only" && done == nil {
				t.Fatal("disabled/source-only lifecycle changed", err)
			}
			provider := &checkpointWorkerProvider{}
			ownerCtx, ownerCancel := context.WithCancel(t.Context())
			defer ownerCancel()
			own, err := engine.StartCheckpointPublications(ownerCtx, map[string]reviewexec.CheckpointProvider{"source-capability": provider})
			if err != nil || own == nil {
				cancel()
				if done != nil {
					waitSourceWorkers(t, done)
				}
				t.Fatal("no publication provider still acquired loop", err)
			}
			ownerCancel()
			waitSourceWorkers(t, own)
			cancel()
			if done != nil {
				waitSourceWorkers(t, done)
			}
			if provider.calls.Load() != 0 || provider.checkpointCalls.Load() != 0 || mode == "source-only" && driver.selections.Load() != 1 {
				t.Fatal("disabled/source-only started provider work")
			}
		})
	}
}
