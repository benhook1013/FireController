package reviewprovider

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// Attribution is diagnostic only: public numeric repository/PR coordinates, never
// provider paths, branch names, commit IDs, titles, credentials or payloads.
type githubTargetKey struct{ repository, pull int64 }
type githubTargetContextKey struct{}

func WithGitHubTarget(ctx context.Context, repository, pull int64) context.Context {
	if ctx == nil || repository <= 0 || !validPullNumber(pull) {
		return ctx
	}
	return context.WithValue(ctx, githubTargetContextKey{}, githubTargetKey{repository, pull})
}

const githubTargetCapacity = 64
const githubTargetReportLimit = 16
const githubTargetReportBytes = 16 * 1024

type githubTargetCount struct {
	Requests    int `json:"requests"`
	Conditional int `json:"conditional"`
	NotModified int `json:"not_modified"`
	Gated       int `json:"local_gate_skips"`
}

func (c *githubTargetCount) add(other githubTargetCount) {
	c.Requests += other.Requests
	c.Conditional += other.Conditional
	c.NotModified += other.NotModified
	c.Gated += other.Gated
}

type githubTargetRow struct {
	Repository int64                        `json:"repository_id"`
	Pull       int64                        `json:"pull_number"`
	Total      githubTargetCount            `json:"total"`
	Callers    map[string]githubTargetCount `json:"callers"`
}

type githubTargetWindow struct {
	rows     map[githubTargetKey]*githubTargetRow
	overflow githubTargetCount
}

// Called under the existing quota mutex. Retains at most 64 targets for the
// current minute; caller names are the fixed WithGitHubCaller vocabulary.
func (w *githubTargetWindow) observe(ctx context.Context, caller string, conditional, gated bool, status int) {
	target, ok := ctx.Value(githubTargetContextKey{}).(githubTargetKey)
	if !ok {
		return
	}
	delta := githubTargetCount{}
	if gated {
		delta.Gated = 1
	} else {
		delta.Requests = 1
	}
	if conditional {
		delta.Conditional = 1
	}
	if conditional && status == http.StatusNotModified {
		delta.NotModified = 1
	}
	row := w.rows[target]
	if row == nil {
		if len(w.rows) >= githubTargetCapacity {
			w.overflow.add(delta)
			return
		}
		if w.rows == nil {
			w.rows = make(map[githubTargetKey]*githubTargetRow)
		}
		row = &githubTargetRow{Repository: target.repository, Pull: target.pull, Callers: make(map[string]githubTargetCount)}
		w.rows[target] = row
	}
	row.Total.add(delta)
	counts := row.Callers[caller]
	counts.add(delta)
	row.Callers[caller] = counts
}

// Report one bounded record, outside the mutex, on ordinary minute rotation.
// Quiet processes retain only one bounded window; no timer, extra I/O or disk
// database is introduced. Existing operator log retention governs emitted logs.
func (w githubTargetWindow) report(ctx context.Context, bucket string, ended time.Time) {
	if len(w.rows) == 0 {
		return
	}
	rows := make([]*githubTargetRow, 0, len(w.rows))
	for _, row := range w.rows {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i].Total.Requests+rows[i].Total.Gated, rows[j].Total.Requests+rows[j].Total.Gated
		if left != right {
			return left > right
		}
		if rows[i].Repository != rows[j].Repository {
			return rows[i].Repository < rows[j].Repository
		}
		return rows[i].Pull < rows[j].Pull
	})
	reported := min(len(rows), githubTargetReportLimit)
	encoded, err := json.Marshal(rows[:reported])
	if err != nil {
		return
	} // fixed numeric/string schema cannot fail marshaling
	for len(encoded) > githubTargetReportBytes && reported > 0 {
		reported--
		encoded, _ = json.Marshal(rows[:reported])
	}
	omitted := githubTargetCount{}
	for _, row := range rows[reported:] {
		omitted.add(row.Total)
	}
	slog.InfoContext(ctx, "GitHub target request totals", "bucket", bucket, "window_ended", ended,
		"captured_targets", len(rows), "reported_targets", reported,
		"omitted_target_totals", omitted, "overflow_totals", w.overflow, "targets", string(encoded))
}
