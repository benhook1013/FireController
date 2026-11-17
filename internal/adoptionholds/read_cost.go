package adoptionholds

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"
)

// Fixed-size, process-local counters only: no project, PR, body, SQL or error text.
// Emission is completion-driven, at most once per minute; an idle final window
// remains in memory until the next completion and is lost on process restart.
type readCost struct{ Records uint64 }
type costWindow struct {
	Event     string `json:"event"`
	WindowMS  int64  `json:"window_ms"`
	Reads     uint64 `json:"reads"`
	Records   uint64 `json:"records_attempted"`
	Errors    uint64 `json:"errors"`
	Deadlines uint64 `json:"deadlines"`
	ElapsedUS int64  `json:"elapsed_us_total"`
	MaxUS     int64  `json:"elapsed_us_max"`
	// Cumulative upper-bound buckets: 1ms, 10ms, 100ms, 1s, +Inf.
	Buckets [5]uint64 `json:"duration_le_buckets"`
}
type costCollector struct {
	sync.Mutex
	since  time.Time
	window costWindow
}

var admissionCosts costCollector

func (c *costCollector) record(now time.Time, elapsed time.Duration, stats readCost, err error) *costWindow {
	c.Lock()
	defer c.Unlock()
	if c.since.IsZero() {
		c.since = now
	}
	w := &c.window
	w.Reads++
	w.Records += stats.Records
	w.ElapsedUS += elapsed.Microseconds()
	if elapsed.Microseconds() > w.MaxUS {
		w.MaxUS = elapsed.Microseconds()
	}
	if err != nil {
		w.Errors++
	}
	if errors.Is(err, context.DeadlineExceeded) {
		w.Deadlines++
	}
	for i, limit := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second} {
		if elapsed <= limit {
			w.Buckets[i]++
		}
	}
	w.Buckets[4]++
	if now.Sub(c.since) < time.Minute {
		return nil
	}
	out := *w
	out.Event = "adoption_read_cost"
	out.WindowMS = now.Sub(c.since).Milliseconds()
	c.since = now
	c.window = costWindow{}
	return &out
}
func observeReadCost(started time.Time, stats readCost, err error) {
	now := time.Now()
	if window := admissionCosts.record(now, now.Sub(started), stats, err); window != nil {
		data, _ := json.Marshal(window)
		log.Print(string(data))
	}
}
