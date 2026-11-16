package reviewprovider

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Completion telemetry is separate from immediate response/quota accounting:
// a 200 header can still be followed by a failed body. Fixed labels only.
type githubCompletionKey struct{ caller, endpoint string }
type githubCompletionCount struct {
	Completed         int   `json:"completed"`
	ReadFailed        int   `json:"read_failed"`
	TimedOut          int   `json:"timed_out"`
	Canceled          int   `json:"canceled"`
	BodyFailed        int   `json:"body_failed"`
	DurationMillis    int64 `json:"duration_millis"`
	MaxDurationMillis int64 `json:"max_duration_millis"`
}

func (q *GitHubQuota) observeReadCompletion(ctx context.Context, now time.Time, path string, started time.Time, phase string, err error) {
	if q == nil {
		return
	}
	caller, _ := ctx.Value(githubCallerKey{}).(string)
	if caller == "" {
		caller = "other"
	}
	key := githubCompletionKey{caller, githubEndpointDetail(path)}
	elapsed := max(int64(0), now.Sub(started).Milliseconds())
	q.mu.Lock()
	var previous map[githubCompletionKey]githubCompletionCount
	end := q.completionNext
	if q.completionNext.IsZero() || !now.Before(q.completionNext) {
		previous, q.completions = q.completions, make(map[githubCompletionKey]githubCompletionCount)
		q.completionNext = now.Add(time.Minute)
	}
	row := q.completions[key]
	row.Completed++
	row.DurationMillis += elapsed
	row.MaxDurationMillis = max(row.MaxDurationMillis, elapsed)
	if err != nil {
		row.ReadFailed++
		var failure *Error
		if errors.As(err, &failure) {
			if failure.Status == http.StatusRequestTimeout {
				row.TimedOut++
			}
			if errors.Is(failure.cause, context.Canceled) {
				row.Canceled++
			}
			if phase == "body" && (failure.transport || failure.cause != nil) {
				row.BodyFailed++
			}
		}
	}
	q.completions[key] = row
	q.mu.Unlock()
	for key, row := range previous {
		slog.InfoContext(ctx, "GitHub read completion totals", "bucket", q.bucket, "window_ended", end, "caller", key.caller, "endpoint_detail", key.endpoint, "completion", row)
	}
}
