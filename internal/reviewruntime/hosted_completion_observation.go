package reviewruntime

import (
	"context"
	"reflect"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Compare only with the latest post-trigger source, never an allowance baseline
// or an older matching observation. Provider timestamps, bodies, identities and
// collection completeness remain significant; only local collection times vary.
// Reusing the exact original record preserves its honest original observation
// time rather than manufacturing a new raw capture with rewritten timestamps.
func reuseHostedCompletionObservation(ctx context.Context, captures *HostedCaptureStore, attempt reviewexec.Attempt, observations []reviewexec.HostedObservation, current HostedCaptureRecord) (HostedCaptureRecord, error) {
	if !validHostedCapture(attempt.ID, current) {
		return current, captureError("validation_failed")
	}
	if len(observations) == 0 || observations[len(observations)-1].Input.Kind != "" {
		return current, nil
	}
	in := observations[len(observations)-1].Input
	previous, err := captures.Read(ctx, attempt.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
	if err != nil {
		return current, err
	}
	if in.StartedAt != previous.StartedAt.Format(time.RFC3339Nano) || in.FinishedAt != previous.FinishedAt.Format(time.RFC3339Nano) || in.ObservedAt != previous.Snapshot.ObservedAt.Format(time.RFC3339Nano) {
		return current, captureError("capture_unavailable")
	}
	left, right := previous, current
	left.StartedAt, left.FinishedAt, left.Snapshot.ObservedAt = time.Time{}, time.Time{}, time.Time{}
	right.StartedAt, right.FinishedAt, right.Snapshot.ObservedAt = time.Time{}, time.Time{}, time.Time{}
	if reflect.DeepEqual(left, right) {
		return previous, nil
	}
	return current, nil
}
