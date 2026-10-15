package main

import (
	"context"
	"sync"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

type reviewRecoveryStarter interface {
	StartRecovery(context.Context) (<-chan error, error)
}

// Startup recovery commits before any observer starts or HTTP accepts work.
// The returned owner waits for every supervisor/observer during shutdown.
func startReviewWorkers(ctx context.Context, execution *reviewexec.Engine, drivers map[string]reviewexec.Driver) (<-chan error, error) {
	if len(drivers) == 0 {
		return nil, execution.RecoverInterrupted(ctx)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	supervisor, err := execution.Start(workerCtx, drivers)
	if err != nil {
		cancel()
		return nil, err
	}
	channels := []<-chan error{supervisor}
	var startupErr error
	for _, driver := range drivers {
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
			channels = append(channels, done)
		}
	}
	result := make(chan error, 1)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var failure error
	for _, done := range channels {
		workers.Go(func() {
			err := <-done
			if err != nil {
				mu.Lock()
				if failure == nil {
					failure = err
				}
				mu.Unlock()
			}
			// An observer/supervisor ending owns cancellation of all siblings;
			// normal daemon cancellation also takes this same bounded path.
			cancel()
		})
	}
	go func() {
		workers.Wait()
		result <- failure
		close(result)
	}()
	return result, startupErr
}
