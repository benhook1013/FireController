package backup

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Only fixed stage names and aggregate quantities enter diagnostics. Never log
// errors, paths, manifests, identities or capture content.
type createDiagnostics struct {
	ctx           context.Context
	phase         string
	started       time.Time
	databaseBytes int64
	captureBytes  int64
	captures      int
}

func beginCreateDiagnostics(ctx context.Context, phase string) *createDiagnostics {
	// Preserve existing nil-context refusal without diagnostics panicking.
	if ctx == nil {
		ctx = context.Background()
	}
	d := &createDiagnostics{ctx: ctx}
	d.start(phase)
	return d
}
func (d *createDiagnostics) start(phase string) {
	d.phase, d.started = phase, time.Now()
	slog.InfoContext(context.WithoutCancel(d.ctx), "Backup creation phase started", "phase", phase)
}
func (d *createDiagnostics) next(phase string) {
	d.finish(nil)
	d.start(phase)
}
func (d *createDiagnostics) finish(err error) {
	slog.InfoContext(context.WithoutCancel(d.ctx), "Backup creation phase outcome",
		"phase", d.phase, "duration_ms", time.Since(d.started).Milliseconds(),
		"database_bytes", d.databaseBytes, "captures_copied", d.captures, "capture_bytes_copied", d.captureBytes,
		"failed", err != nil, "deadline_exceeded", errors.Is(d.ctx.Err(), context.DeadlineExceeded),
		"cancelled", errors.Is(d.ctx.Err(), context.Canceled))
}
