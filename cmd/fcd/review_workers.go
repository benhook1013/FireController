package main

import (
	"context"
	"io"
	"sync"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

type reviewRecoveryStarter interface {
	StartRecovery(context.Context) (<-chan error, error)
}

type sourceReaderProvider interface {
	SourceReader() reviewexec.TargetReader
}

// Startup recovery commits before any observer starts or HTTP accepts work.
// The returned owner joins the supervisor, source worker and observers during shutdown.
func startReviewWorkers(ctx context.Context, execution *reviewexec.Engine, drivers map[string]reviewexec.Driver, diagnostics io.Writer) (<-chan error, error) {
	if len(drivers) == 0 {
		return nil, execution.RecoverInterrupted(ctx)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	supervisor, err := execution.Start(workerCtx, drivers)
	if err != nil {
		cancel()
		return nil, err
	}
	type worker struct {
		done             <-chan error
		recoveryObserver bool
		capabilityID     string
	}
	channels := []worker{{done: supervisor}}
	var startupErr error
	for capabilityID, driver := range drivers {
		if starter, ok := driver.(reviewRecoveryStarter); ok {
			done, err := starter.StartRecovery(workerCtx)
			if err != nil || done == nil {
				startupErr = err
				if startupErr == nil {
					startupErr = core.Fail("configuration_invalid", "The native recovery observer could not be started.", 500)
				}
				cancel()
				break
			}
			channels = append(channels, worker{done: done, recoveryObserver: true, capabilityID: capabilityID})
		}
	}
	if startupErr == nil {
		readers := make(map[string]reviewexec.TargetReader)
		for id, driver := range drivers {
			if provider, ok := driver.(sourceReaderProvider); ok {
				readers[id] = provider.SourceReader()
			}
		}
		done, err := execution.StartSourceRefresh(workerCtx, readers)
		if err != nil {
			startupErr = err
			cancel()
		} else if done != nil {
			channels = append(channels, worker{done: done})
		}
	}
	result := make(chan error, 1)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var failure error
	for _, owned := range channels {
		workers.Go(func() {
			err := <-owned.done
			if owned.recoveryObserver {
				// An observer has no authority to settle uncertain work or stop
				// unrelated coordination. Report only its unexpected termination,
				// never the retained error's provider text or private details.
				if workerCtx.Err() == nil {
					mu.Lock()
					diagnostic(diagnostics, "review_recovery_stopped", "The native hosted recovery observer for capability "+owned.capabilityID+" stopped; inspect retained uncertain work before recovery.")
					mu.Unlock()
				}
				return
			}
			if err != nil {
				mu.Lock()
				if failure == nil {
					failure = err
				}
				mu.Unlock()
			}
			// The supervisor remains fatal; normal daemon cancellation joins
			// every observer through the same owner without restarting it.
			cancel()
		})
	}
	go func() {
		defer cancel()
		workers.Wait()
		result <- failure
		close(result)
	}()
	return result, startupErr
}
