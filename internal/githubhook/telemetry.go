package githubhook

import (
	"context"
	"log/slog"
	"time"
)

type hookOutcomeKey struct{ event, outcome string }
type hookTelemetry struct {
	next time.Time
	rows map[hookOutcomeKey]uint64
}

// Match the request telemetry's event-driven minute rollover. This introduces
// no timer, provider request, durable history or delivery/payload identifiers.
func (h *Handler) observeOutcome(ctx context.Context, event, outcome string) {
	if !knownEvent(event) && event != "other" {
		event = "unverified"
	}
	switch outcome {
	case "rejected_request", "rejected_signature", "ignored", "conflict", "inflight", "duplicate", "capacity", "callback_failed", "admitted":
	default:
		outcome = "rejected_request"
	}
	h.mu.Lock()
	now := h.now()
	var previous map[hookOutcomeKey]uint64
	end := h.telemetry.next
	if end.IsZero() || !now.Before(end) {
		previous = h.telemetry.rows
		h.telemetry.rows = make(map[hookOutcomeKey]uint64)
		h.telemetry.next = now.Add(time.Minute)
	}
	key := hookOutcomeKey{event, outcome}
	h.telemetry.rows[key]++
	h.mu.Unlock()
	for key, count := range previous {
		slog.InfoContext(ctx, "GitHub webhook delivery totals", "window_ended", end, "event", key.event, "outcome", key.outcome, "deliveries", count)
	}
}
