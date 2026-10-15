package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type managedBackupRunner interface {
	Run(context.Context, func(error)) error
}

// No worker is launched for the disabled configuration. An enabled manager
// owns its serial attempt loop; per-attempt failures are diagnostic callbacks.
func startBackupWorker(ctx context.Context, manager managedBackupRunner, enabled bool, report func(error)) <-chan error {
	if !enabled {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- manager.Run(ctx, report)
	}()
	return done
}

func waitBackupWorker(done <-chan error, timeout time.Duration) error {
	if done == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return core.Fail("shutdown_failed", "The managed backup worker did not settle within the shutdown budget; inspect retained backup status before restarting.", 500)
	}
}

// Background attempts and foreground shutdown may both emit diagnostics.
// Serialize each JSON write without holding a lock during backup work.
type daemonDiagnosticWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *daemonDiagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}
