package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

type batchTelemetryHandler struct {
	telemetry *lifecycleBatchTelemetry
	inner     slog.Handler
	held      bool
}

func (h *batchTelemetryHandler) Enabled(c context.Context, l slog.Level) bool {
	return h.inner.Enabled(c, l)
}
func (h *batchTelemetryHandler) Handle(c context.Context, r slog.Record) error {
	if h.telemetry.mu.TryLock() {
		h.telemetry.mu.Unlock()
	} else {
		h.held = true
	}
	return h.inner.Handle(c, r)
}
func (h *batchTelemetryHandler) WithAttrs(a []slog.Attr) slog.Handler { return h.inner.WithAttrs(a) }
func (h *batchTelemetryHandler) WithGroup(g string) slog.Handler      { return h.inner.WithGroup(g) }
func TestLifecycleBatchTelemetryBoundedAndAfterWork(t *testing.T) {
	var telemetry lifecycleBatchTelemetry
	var output bytes.Buffer
	handler := &batchTelemetryHandler{telemetry: &telemetry, inner: slog.NewJSONHandler(&output, nil)}
	prior := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prior)
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	telemetry.record(now, batchOutcomeCount)
	if !telemetry.started.IsZero() {
		t.Fatal("invalid outcome initialized telemetry")
	}
	telemetry.record(now, batchEligible)
	telemetry.record(now, batchPrepared)
	telemetry.record(now, batchPublishedMember)
	telemetry.record(now, batchPublishedMember)
	if output.Len() != 0 {
		t.Fatal("recording logged during provider work")
	}
	telemetry.report(now.Add(59 * time.Second))
	if output.Len() != 0 {
		t.Fatal("short window logged")
	}
	telemetry.report(now.Add(time.Minute))
	if handler.held {
		t.Fatal("logging held telemetry lock")
	}
	var row map[string]any
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["eligible_cohorts"] != float64(1) || row["prepared_cohorts"] != float64(1) || row["published_members"] != float64(2) {
		t.Fatal(row)
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "window_started": true, "window_ended": true, "eligible_cohorts": true, "existing_watch_fallback_cohorts": true, "prepared_cohorts": true, "account_aborted_cohorts": true, "prepare_refused_cohorts": true, "published_members": true, "discarded_members": true}
	for key := range row {
		if !allowed[key] {
			t.Fatal("unbounded/private label", key)
		}
	}
	output.Reset()
	telemetry.report(now.Add(2 * time.Minute))
	if output.Len() != 0 {
		t.Fatal("empty window logged")
	}
}
