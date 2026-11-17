package reviewprovider

import (
	"context"
	"log/slog"
	"time"
)

// Fixed endpoint labels, one bounded per-adapter window, no selectors or bodies.
// Pressure counters describe why an insertion evicted entries, not which PR.
type conditionalCount struct {
	Hits               int `json:"hits"`
	Misses             int `json:"misses"`
	Responses200       int `json:"responses_200"`
	ETagPresent        int `json:"etag_present"`
	NoETag             int `json:"no_etag"`
	InvalidETag        int `json:"invalid_etag"`
	NoStore            int `json:"no_store"`
	InvalidJSON        int `json:"invalid_json"`
	Oversize           int `json:"oversize"`
	Stored             int `json:"stored"`
	Evictions          int `json:"evictions"`
	BytePressure       int `json:"byte_pressure"`
	EntryPressure      int `json:"entry_pressure"`
	TerminalPageBypass int `json:"terminal_page_bypass"`
}

func (g *GitHub) recordConditional(path, event string) {
	if g.quota == nil {
		return
	}
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	g.recordConditionalLocked(path, event)
}
func (g *GitHub) recordConditionalLocked(path, event string) {
	if g.quota == nil {
		return
	}
	if g.conditionalCounts == nil {
		g.conditionalCounts = make(map[string]conditionalCount)
	}
	if g.conditionalNext.IsZero() {
		g.conditionalNext = time.Now().Add(time.Minute)
	}
	key := githubEndpointDetail(path)
	row := g.conditionalCounts[key]
	switch event {
	case "hit":
		row.Hits++
	case "miss":
		row.Misses++
	case "response_200":
		row.Responses200++
	case "etag_present":
		row.ETagPresent++
	case "no_etag":
		row.NoETag++
	case "invalid_etag":
		row.InvalidETag++
	case "no_store":
		row.NoStore++
	case "invalid_json":
		row.InvalidJSON++
	case "oversize":
		row.Oversize++
	case "stored":
		row.Stored++
	case "eviction":
		row.Evictions++
	case "byte_pressure":
		row.BytePressure++
	case "entry_pressure":
		row.EntryPressure++
	case "terminal_page_bypass":
		row.TerminalPageBypass++
	default:
		return
	}
	g.conditionalCounts[key] = row
}
func (g *GitHub) reportConditional(ctx context.Context, now time.Time) {
	if g.quota == nil {
		return
	}
	g.cacheMu.Lock()
	if g.conditionalNext.IsZero() || now.Before(g.conditionalNext) {
		g.cacheMu.Unlock()
		return
	}
	rows, end := g.conditionalCounts, g.conditionalNext
	entries, retained := len(g.cache), g.cacheBytes
	g.conditionalCounts = nil
	g.conditionalNext = now.Add(time.Minute)
	g.cacheMu.Unlock()
	for endpoint, row := range rows {
		slog.InfoContext(ctx, "GitHub conditional cache totals", "bucket", g.quota.bucket, "window_ended", end, "endpoint_detail", endpoint, "cache", row, "retained_entries", entries, "retained_bytes", retained)
	}
}
