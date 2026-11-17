package offsite

import (
	"context"
	"log/slog"
	"time"
)

// Fixed phase names and numeric indices only. No paths, remote names, receipt
// identities, credential data, payloads or provider error strings are logged.
func beginBatchPhase(ctx context.Context, destination int, phase string) func(error) {
	started := time.Now()
	slog.InfoContext(context.WithoutCancel(ctx), "Offsite batch phase started", "destination", destination, "phase", phase)
	return func(err error) {
		slog.InfoContext(context.WithoutCancel(ctx), "Offsite batch phase outcome", "destination", destination, "phase", phase, "duration_ms", time.Since(started).Milliseconds(), "failed", err != nil, "cancelled", ctx.Err() != nil)
	}
}

type batchTransportTiming struct {
	calls    int
	failures int
	elapsed  time.Duration
}
type batchTimedTransport struct {
	transport
	timings [4]batchTransportTiming
}

func (t *batchTimedTransport) record(op int, started time.Time, err error) {
	t.timings[op].calls++
	t.timings[op].elapsed += time.Since(started)
	if err != nil {
		t.timings[op].failures++
	}
}
func (t *batchTimedTransport) read(ctx context.Context, name string) (data []byte, err error) {
	started := time.Now()
	defer func() { t.record(0, started, err) }()
	return t.transport.read(ctx, name)
}
func (t *batchTimedTransport) put(ctx context.Context, local, name string) (err error) {
	started := time.Now()
	defer func() { t.record(1, started, err) }()
	return t.transport.put(ctx, local, name)
}
func (t *batchTimedTransport) get(ctx context.Context, name, local string, limits ...int64) (err error) {
	started := time.Now()
	defer func() { t.record(2, started, err) }()
	return t.transport.get(ctx, name, local, limits...)
}
func (t *batchTimedTransport) remove(ctx context.Context, name string) (err error) {
	started := time.Now()
	defer func() { t.record(3, started, err) }()
	return t.transport.remove(ctx, name)
}
func (t *batchTimedTransport) report(ctx context.Context, destination int) {
	for i, op := range []string{"read_receipt", "upload", "download", "delete_owned"} {
		m := t.timings[i]
		slog.InfoContext(context.WithoutCancel(ctx), "Offsite batch transport totals", "destination", destination, "operation", op, "calls", m.calls, "failures", m.failures, "duration_ms", m.elapsed.Milliseconds())
	}
}
