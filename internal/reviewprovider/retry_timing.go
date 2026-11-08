package reviewprovider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryTimingEvidence preserves a response's exact provider floor separately
// from RetryAfterSeconds, which is a legacy rounded, capped scheduling hint.
// State is proven, unknown, or unsupported. Only proven has NotBefore set.
// This evidence does not prove availability, a read-only request, authorization,
// or a safe retry. Consumers must independently establish all those boundaries.
type RetryTimingEvidence struct {
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
	NotBefore  time.Time `json:"not_before,omitempty"`
}

// Keep the supported scheduling window bounded without turning a longer floor
// into an earlier one. Ambiguous headers cannot establish exact timing.
func retryTiming(header http.Header, now time.Time) RetryTimingEvidence {
	now = now.UTC()
	unknown := RetryTimingEvidence{State: "unknown", ObservedAt: now}
	unsupported := RetryTimingEvidence{State: "unsupported", ObservedAt: now}
	var due time.Time
	values := header.Values("Retry-After")
	if len(values) > 1 {
		return unknown
	}
	if len(values) == 1 {
		value := strings.TrimSpace(values[0])
		if value == "" {
			return unknown
		}
		if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
			if seconds > 86400 {
				return unsupported
			}
			due = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(value); err == nil {
			due = parsed.UTC()
		} else {
			// An overflowing unsigned decimal is a real but unsupported long delay.
			decimal := true
			for _, c := range value {
				if c < '0' || c > '9' {
					decimal = false
					break
				}
			}
			if decimal {
				return unsupported
			}
			return unknown
		}
	}
	remaining := header.Values("X-RateLimit-Remaining")
	if len(remaining) > 1 {
		return unknown
	}
	if len(remaining) == 1 && strings.TrimSpace(remaining[0]) == "0" {
		reset := header.Values("X-RateLimit-Reset")
		if len(reset) != 1 {
			return unknown
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(reset[0]), 10, 64)
		if err != nil || seconds <= 0 {
			return unknown
		}
		// Compare seconds before constructing a potentially overflowing time.
		if seconds > now.Add(24*time.Hour).Unix() {
			return unsupported
		}
		floor := time.Unix(seconds, 0).UTC()
		if floor.After(due) {
			due = floor
		}
	}
	if due.IsZero() {
		return unknown
	}
	if due.After(now.Add(24 * time.Hour)) {
		return unsupported
	}
	// An expired valid floor remains proven, not a promise that a request works.
	return RetryTimingEvidence{State: "proven", ObservedAt: now, NotBefore: due}
}
