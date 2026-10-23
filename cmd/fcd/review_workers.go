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

type cliRecoveryStarter interface {
	StartCLIRecovery(context.Context) (<-chan error, error)
}

type sourceReaderProvider interface {
	SourceReader() reviewexec.TargetReader
}

type externalHostedSourceProvider interface {
	HostedObservationReader() reviewexec.ExternalHostedReader
}

func lifecycleReader(driver reviewexec.Driver, fallback reviewexec.TargetReader) reviewexec.TargetReader {
	if provider, ok := driver.(externalHostedSourceProvider); ok {
		if reader := provider.HostedObservationReader(); reader != nil {
			return reader
		}
	}
	if combined, ok := driver.(*reviewChannelDriver); ok && combined.hosted != nil {
		if reader := combined.hosted.HostedObservationReader(); reader != nil {
			return reader
		}
	}
	return fallback
}

// Review-attempt recovery commits before any observer starts. All worker
// startup completes before HTTP accepts work; the returned owner joins the
// supervisor, source/publication workers and observers.
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
		if starter, ok := driver.(cliRecoveryStarter); ok {
			done, err := starter.StartCLIRecovery(workerCtx)
			if err != nil || done == nil {
				startupErr = err
				if startupErr == nil {
					startupErr = core.Fail("configuration_invalid", "The retained CLI recovery observer could not be started.", 500)
				}
				cancel()
				break
			}
			channels = append(channels, worker{done: done, recoveryObserver: true, capabilityID: capabilityID})
		}
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
		publications := make(map[string]reviewexec.CheckpointProvider)
		for id, driver := range drivers {
			if provider, ok := driver.(sourceReaderProvider); ok {
				reader := provider.SourceReader()
				readers[id] = reader
				if publisher, ok := reader.(reviewexec.CheckpointProvider); ok {
					publications[id] = publisher
				}
			}
		}
		done, err := execution.StartReviewRuns(workerCtx, readers)
		if err != nil {
			startupErr = err
			cancel()
		} else if done != nil {
			channels = append(channels, worker{done: done})
		}
		if startupErr == nil {
			done, err = execution.StartSourceRefresh(workerCtx, readers)
			if err != nil {
				startupErr = err
				cancel()
			} else if done != nil {
				channels = append(channels, worker{done: done})
			}
		}
		if startupErr == nil {
			lifecycleReaders := make(map[string]reviewexec.TargetReader, len(readers))
			for id, reader := range readers {
				lifecycleReaders[id] = lifecycleReader(drivers[id], reader)
			}
			done, err = execution.StartLifecycleSync(workerCtx, lifecycleReaders)
			if err != nil {
				startupErr = err
				cancel()
			} else if done != nil {
				channels = append(channels, worker{done: done})
			}
		}
		if startupErr == nil {
			done, err = execution.StartCheckpointPublications(workerCtx, publications)
			if err != nil {
				startupErr = err
				cancel()
			} else if done != nil {
				channels = append(channels, worker{done: done})
			}
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
					diagnostic(diagnostics, "review_recovery_stopped", "The native review recovery observer for capability "+owned.capabilityID+" stopped; inspect retained uncertain work before recovery.")
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
