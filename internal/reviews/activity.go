package reviews

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/benhook1013/FireController/internal/core"
)

const activityRowLimit = 10000

type ActivityCounts struct {
	Hosted int64 `json:"hosted"`
	CLI    int64 `json:"cli"`
}
type ActivityAverages struct {
	Hosted float64 `json:"hosted"`
	CLI    float64 `json:"cli"`
}
type ActivityBucket struct {
	Since   string `json:"since"`
	Until   string `json:"until"`
	Label   string `json:"label"`
	Partial bool   `json:"partial"`
	Hosted  int64  `json:"hosted"`
	CLI     int64  `json:"cli"`
}
type ActivityDiagnostics struct {
	Failed      int64 `json:"failed"`
	Held        int64 `json:"held"`
	Interrupted int64 `json:"interrupted"`
	Unproven    int64 `json:"unproven"`
}
type ActivityCoverage struct {
	Basis                     string   `json:"basis"`
	UndatedResults            int64    `json:"undated_results"`
	UndatedAttempts           int64    `json:"undated_attempts"`
	ExcludedIncompleteResults int64    `json:"excluded_incomplete_results"`
	Limits                    []string `json:"limits"`
}
type ActivityResponse struct {
	ProjectID       string              `json:"project_id"`
	AsOf            string              `json:"as_of"`
	Since           string              `json:"since"`
	Until           string              `json:"until"`
	Timezone        string              `json:"timezone"`
	PullNumber      int64               `json:"pull_number,omitempty"`
	Totals          ActivityCounts      `json:"totals"`
	AveragesPerHour ActivityAverages    `json:"averages_per_hour"`
	Buckets         []ActivityBucket    `json:"buckets"`
	Diagnostics     ActivityDiagnostics `json:"diagnostics"`
	Coverage        ActivityCoverage    `json:"coverage"`
}
type activityWindow struct {
	since, until time.Time
	zone         string
	location     *time.Location
	pull         int64
}

func activityQuery(r *http.Request, asOf time.Time) (activityWindow, error) {
	var selected activityWindow
	q, err := core.QueryKeys(r, "validation_failed", "hours", "since", "until", "timezone", "pull_number")
	if err != nil {
		return selected, err
	}
	invalid := func() (activityWindow, error) {
		return selected, validationError("Use hours from 1 to 168, or both since/until RFC3339 timestamps at most seven days apart, a valid IANA timezone and an optional positive pull_number.")
	}
	if len(r.URL.RawQuery) > 4096 {
		return invalid()
	}
	selected.zone = "UTC"
	if _, ok := q["timezone"]; ok {
		selected.zone = q.Get("timezone")
	}
	if !validText(selected.zone, 128, true) || strings.TrimSpace(selected.zone) != selected.zone || selected.zone == "Local" {
		return invalid()
	}
	selected.location, err = time.LoadLocation(selected.zone)
	if err != nil {
		return invalid()
	}
	if _, ok := q["pull_number"]; ok {
		selected.pull, err = strconv.ParseInt(q.Get("pull_number"), 10, 32)
		if err != nil || selected.pull < 1 || strconv.FormatInt(selected.pull, 10) != q.Get("pull_number") {
			return invalid()
		}
	}
	_, hasSince := q["since"]
	_, hasUntil := q["until"]
	_, hasHours := q["hours"]
	if hasSince || hasUntil {
		if !hasSince || !hasUntil || hasHours {
			return invalid()
		}
		selected.since, err = parseTimestamp(q.Get("since"), true)
		if err != nil {
			return invalid()
		}
		selected.until, err = parseTimestamp(q.Get("until"), true)
		if err != nil {
			return invalid()
		}
	} else {
		hours := 24
		if hasHours {
			hours, err = strconv.Atoi(q.Get("hours"))
			if err != nil || strconv.Itoa(hours) != q.Get("hours") || hours < 1 || hours > 168 {
				return invalid()
			}
		}
		selected.until = asOf
		selected.since = asOf.Add(-time.Duration(hours) * time.Hour)
	}
	selected.since, selected.until = selected.since.UTC(), selected.until.UTC()
	if selected.since.IsZero() || selected.until.IsZero() || !selected.since.Before(selected.until) || selected.until.Sub(selected.since) > 7*24*time.Hour || selected.until.After(asOf.Add(5*time.Second)) {
		return invalid()
	}
	return selected, nil
}

func newActivity(project string, asOf time.Time, w activityWindow) ActivityResponse {
	result := ActivityResponse{ProjectID: project, AsOf: asOf.Format(time.RFC3339Nano), Since: w.since.Format(time.RFC3339Nano), Until: w.until.Format(time.RFC3339Nano), Timezone: w.zone, PullNumber: w.pull, Buckets: []ActivityBucket{}, Coverage: ActivityCoverage{Basis: "current_retained_results", Limits: []string{
		"Current retained source records only; this is not an exhaustive provider inventory, quota measurement or native round credit.",
		"Original producer identities remain distinct; only existing proven native/observer reconciliation removes duplicate projections.",
		"Buckets use UTC-aligned elapsed hours with local timezone/offset labels. Complete source inventory may still have source-specific coverage limitations.",
	}}}
	for start := w.since.Truncate(time.Hour); start.Before(w.until); start = start.Add(time.Hour) {
		since, until := start, start.Add(time.Hour)
		if since.Before(w.since) {
			since = w.since
		}
		if until.After(w.until) {
			until = w.until
		}
		result.Buckets = append(result.Buckets, ActivityBucket{Since: since.Format(time.RFC3339Nano), Until: until.Format(time.RFC3339Nano), Label: start.In(w.location).Format(time.RFC3339), Partial: !since.Equal(start) || !until.Equal(start.Add(time.Hour))})
	}
	return result
}

func activity(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		asOf := time.Now().UTC()
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		selected, err := activityQuery(r, asOf)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := newActivity(scope.ProjectID, asOf, selected)
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_workspace WHERE project_id=?)`, scope.ProjectID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return nil
			}
			if err := activityRuns(ctx, tx, scope.ProjectID, selected, &result); err != nil {
				return err
			}
			return activityAttempts(ctx, tx, scope.ProjectID, selected, &result)
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		elapsed := selected.until.Sub(selected.since).Hours()
		result.AveragesPerHour = ActivityAverages{Hosted: float64(result.Totals.Hosted) / elapsed, CLI: float64(result.Totals.CLI) / elapsed}
		if result.Coverage.UndatedResults > 0 || result.Coverage.UndatedAttempts > 0 {
			result.Coverage.Limits = append(result.Coverage.Limits, "Undated counts cover the selected project's current records, not a known position in this time window; no snapshot or start date was substituted.")
		}
		core.WriteJSON(w, http.StatusOK, result)
	}
}

const activityRunsFrom = ` FROM review_current_runs c
 CROSS JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision AND m.pull_number=c.pull_number AND m.run_order=c.run_order
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision AND v.pull_number=m.pull_number
 WHERE c.project_id=? AND v.channel IN('hosted','cli') AND v.outcome='completed'`
const activityAttemptsFrom = ` FROM review_current_attempts c
 CROSS JOIN review_attempt_memberships m ON m.project_id=c.project_id AND m.source=c.source AND m.source_attempt_id=c.source_attempt_id AND m.start_revision=c.start_revision AND m.pull_number=c.pull_number AND m.position=c.position
 CROSS JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision AND v.pull_number=m.pull_number
 WHERE c.project_id=? AND v.channel IN('hosted','cli') AND v.state IN('failed','held','rate_limited','uncertain','unknown','skipped','cancelled')`

func activitySelection(from, project string, w activityWindow) (string, []any) {
	args := []any{project}
	if w.pull != 0 {
		from += " AND c.pull_number=?"
		args = append(args, w.pull)
	}
	return from, args
}
func activityRange(from string, args []any, w activityWindow) (string, []any) {
	// SQLite's date representation has less precision than retained timestamps.
	// This broad candidate window is followed by exact nanosecond UTC checks.
	// Retained RFC3339 offsets accepted by Go can exceed SQLite's timezone
	// range. Include those nonempty values for the same bounded exact check.
	return from + ` AND v.finished_at<>'' AND (julianday(v.finished_at) IS NULL OR (julianday(v.finished_at)>=julianday(?) AND julianday(v.finished_at)<julianday(?))) LIMIT 10001`, append(args, w.since.Add(-time.Second).Format(time.RFC3339Nano), w.until.Add(time.Second).Format(time.RFC3339Nano))
}
func activityLimit() error {
	return core.Fail("review_activity_limit", "This activity window contains too many retained result or attempt candidates. Narrow the time window or select pull_number.", http.StatusConflict)
}
func activityRuns(ctx context.Context, tx *sql.Tx, project string, w activityWindow, result *ActivityResponse) error {
	from, args := activitySelection(activityRunsFrom, project, w)
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+strings.Replace(from, " AND v.outcome='completed'", "", 1)+` AND v.finished_at=''`, args...).Scan(&result.Coverage.UndatedResults); err != nil {
		return err
	}
	query, rangeArgs := activityRange(from, args, w)
	rows, err := tx.QueryContext(ctx, `SELECT v.channel,v.complete,CASE WHEN length(CAST(v.finished_at AS BLOB))<=64 THEN v.finished_at END`+query, rangeArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > activityRowLimit {
			return activityLimit()
		}
		var channel, finished string
		var complete bool
		if err := rows.Scan(&channel, &complete, &finished); err != nil {
			return err
		}
		at, err := parseTimestamp(finished, true)
		if err != nil {
			return historyInconsistent()
		}
		if at.Before(w.since) || !at.Before(w.until) {
			continue
		}
		if !complete {
			result.Coverage.ExcludedIncompleteResults++
			continue
		}
		index := int(at.UTC().Sub(w.since.Truncate(time.Hour)) / time.Hour)
		if channel == "hosted" {
			result.Totals.Hosted++
			result.Buckets[index].Hosted++
		} else {
			result.Totals.CLI++
			result.Buckets[index].CLI++
		}
	}
	return rows.Err()
}
func activityAttempts(ctx context.Context, tx *sql.Tx, project string, w activityWindow, result *ActivityResponse) error {
	from, args := activitySelection(activityAttemptsFrom, project, w)
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+from+` AND v.finished_at=''`, args...).Scan(&result.Coverage.UndatedAttempts); err != nil {
		return err
	}
	query, rangeArgs := activityRange(from, args, w)
	rows, err := tx.QueryContext(ctx, `SELECT v.state,v.reason,CASE WHEN length(CAST(v.finished_at AS BLOB))<=64 THEN v.finished_at END`+query, rangeArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > activityRowLimit {
			return activityLimit()
		}
		var state, reason, finished string
		if err := rows.Scan(&state, &reason, &finished); err != nil {
			return err
		}
		at, err := parseTimestamp(finished, true)
		if err != nil {
			return historyInconsistent()
		}
		if at.Before(w.since) || !at.Before(w.until) {
			continue
		}
		switch {
		case reason == "execution_interrupted":
			result.Diagnostics.Interrupted++
		case state == "failed":
			result.Diagnostics.Failed++
		case state == "held" || state == "rate_limited":
			result.Diagnostics.Held++
		default:
			result.Diagnostics.Unproven++
		}
	}
	return rows.Err()
}
