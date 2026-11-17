package client

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type ReviewActivityOptions struct {
	Hours      int    `json:"hours,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
	PullNumber int64  `json:"pull_number,omitempty"`
}
type ReviewActivityCounts struct {
	Hosted int64 `json:"hosted"`
	CLI    int64 `json:"cli"`
}
type ReviewActivityAverages struct {
	Hosted float64 `json:"hosted"`
	CLI    float64 `json:"cli"`
}
type ReviewActivityBucket struct {
	Since   string `json:"since"`
	Until   string `json:"until"`
	Label   string `json:"label"`
	Partial bool   `json:"partial"`
	Hosted  int64  `json:"hosted"`
	CLI     int64  `json:"cli"`
}
type ReviewActivityDiagnostics struct {
	Failed      int64 `json:"failed"`
	Held        int64 `json:"held"`
	Interrupted int64 `json:"interrupted"`
	Unproven    int64 `json:"unproven"`
}
type ReviewActivityCoverage struct {
	Basis                     string   `json:"basis"`
	UndatedResults            int64    `json:"undated_results"`
	UndatedAttempts           int64    `json:"undated_attempts"`
	ExcludedIncompleteResults int64    `json:"excluded_incomplete_results"`
	Limits                    []string `json:"limits"`
}
type ReviewActivity struct {
	ProjectID       string                    `json:"project_id"`
	AsOf            string                    `json:"as_of"`
	Since           string                    `json:"since"`
	Until           string                    `json:"until"`
	Timezone        string                    `json:"timezone"`
	PullNumber      int64                     `json:"pull_number,omitempty"`
	Totals          ReviewActivityCounts      `json:"totals"`
	AveragesPerHour ReviewActivityAverages    `json:"averages_per_hour"`
	Buckets         []ReviewActivityBucket    `json:"buckets"`
	Diagnostics     ReviewActivityDiagnostics `json:"diagnostics"`
	Coverage        ReviewActivityCoverage    `json:"coverage"`
}

func activityLocation(zone string) (*time.Location, error) {
	if zone == "" {
		zone = "UTC"
	}
	if zone == "Local" || len(zone) > 128 || strings.TrimSpace(zone) != zone {
		return nil, validation()
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, validation()
	}
	return location, nil
}

// ValidateReviewActivityOptions performs local validation without service work.
func ValidateReviewActivityOptions(o ReviewActivityOptions) error {
	if _, err := activityLocation(o.Timezone); err != nil || o.Hours < 0 || o.Hours > 168 || len(o.Since) > 64 || len(o.Until) > 64 || o.PullNumber != 0 && !validPullNumber(o.PullNumber) {
		return validation()
	}
	if o.Since != "" || o.Until != "" {
		since, a := time.Parse(time.RFC3339Nano, o.Since)
		until, b := time.Parse(time.RFC3339Nano, o.Until)
		if o.Hours != 0 || a != nil || b != nil || since.IsZero() || until.IsZero() || !since.Before(until) || until.Sub(since) > 7*24*time.Hour || until.After(time.Now().Add(5*time.Second)) {
			return validation()
		}
	}
	return nil
}

// ReviewActivity is one project-scoped retained-source read, without provider
// access, enrollment, configuration or history-page enumeration.
func (c *Client) ReviewActivity(ctx context.Context, options ReviewActivityOptions) (ReviewActivity, error) {
	if ValidateReviewActivityOptions(options) != nil {
		return ReviewActivity{}, validation()
	}
	query := url.Values{}
	if options.Hours != 0 {
		query.Set("hours", strconv.Itoa(options.Hours))
	}
	if options.Since != "" {
		query.Set("since", options.Since)
		query.Set("until", options.Until)
	}
	if options.Timezone != "" {
		query.Set("timezone", options.Timezone)
	}
	if options.PullNumber != 0 {
		query.Set("pull_number", strconv.FormatInt(options.PullNumber, 10))
	}
	var result ReviewActivity
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "activity"), query, true, "", nil, http.StatusOK, false, &result, 128<<10, decodeReviewResponse); err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == http.StatusConflict && failure.Code == "review_activity_limit" {
			limited := *failure
			limited.Message = "The selected activity window contains too many retained result or attempt candidates."
			limited.Action = "Narrow the time window or select one PR with pull_number (CLI: --pr), then request activity again. No automatic retry was performed."
			return ReviewActivity{}, &limited
		}
		return ReviewActivity{}, err
	}
	if result.ProjectID != c.identity.ProjectID || !validReviewActivity(result, options) {
		return ReviewActivity{}, invalidResponse()
	}
	return result, nil
}

func activityTime(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, err == nil && !at.IsZero() && value == at.UTC().Format(time.RFC3339Nano)
}
func activityCount(n int64) bool { return n >= 0 && n <= 10000 }
func activityAverage(actual, expected float64) bool {
	return !math.IsNaN(actual) && !math.IsInf(actual, 0) && math.Abs(actual-expected) <= 1e-9*math.Max(1, math.Abs(expected))
}
func validReviewActivity(v ReviewActivity, o ReviewActivityOptions) bool {
	asOf, a := activityTime(v.AsOf)
	since, b := activityTime(v.Since)
	until, d := activityTime(v.Until)
	zone := o.Timezone
	if zone == "" {
		zone = "UTC"
	}
	location, err := activityLocation(zone)
	if err != nil || !a || !b || !d || !since.Before(until) || until.Sub(since) > 7*24*time.Hour || asOf.After(time.Now().Add(5*time.Second)) || until.After(asOf.Add(5*time.Second)) || v.Timezone != zone || v.PullNumber != o.PullNumber || v.Buckets == nil || len(v.Buckets) < 1 || len(v.Buckets) > 169 {
		return false
	}
	if o.Since != "" {
		wantedSince, _ := time.Parse(time.RFC3339Nano, o.Since)
		wantedUntil, _ := time.Parse(time.RFC3339Nano, o.Until)
		if !since.Equal(wantedSince) || !until.Equal(wantedUntil) {
			return false
		}
	} else {
		hours := o.Hours
		if hours == 0 {
			hours = 24
		}
		if !until.Equal(asOf) || until.Sub(since) != time.Duration(hours)*time.Hour {
			return false
		}
	}
	if !activityCount(v.Totals.Hosted) || !activityCount(v.Totals.CLI) || v.Totals.Hosted+v.Totals.CLI > 10000 {
		return false
	}
	cursor := since
	var hosted, cli int64
	for _, bucket := range v.Buckets {
		start, ok := activityTime(bucket.Since)
		finish, valid := activityTime(bucket.Until)
		slot := start.Truncate(time.Hour)
		expectedUntil := slot.Add(time.Hour)
		if until.Before(expectedUntil) {
			expectedUntil = until
		}
		if !ok || !valid || !start.Equal(cursor) || !start.Before(finish) || !finish.Equal(expectedUntil) || bucket.Label != slot.In(location).Format(time.RFC3339) || bucket.Partial != (!start.Equal(slot) || !finish.Equal(slot.Add(time.Hour))) || !activityCount(bucket.Hosted) || !activityCount(bucket.CLI) {
			return false
		}
		hosted += bucket.Hosted
		cli += bucket.CLI
		cursor = finish
	}
	if !cursor.Equal(until) || hosted != v.Totals.Hosted || cli != v.Totals.CLI {
		return false
	}
	elapsed := until.Sub(since).Hours()
	if !activityAverage(v.AveragesPerHour.Hosted, float64(hosted)/elapsed) || !activityAverage(v.AveragesPerHour.CLI, float64(cli)/elapsed) {
		return false
	}
	counts := v.Diagnostics
	if !activityCount(counts.Failed) || !activityCount(counts.Held) || !activityCount(counts.Interrupted) || !activityCount(counts.Unproven) || counts.Failed+counts.Held+counts.Interrupted+counts.Unproven > 10000 {
		return false
	}
	coverage := v.Coverage
	if coverage.Basis != "current_retained_results" || coverage.UndatedResults < 0 || coverage.UndatedResults > 9007199254740991 || coverage.UndatedAttempts < 0 || coverage.UndatedAttempts > 9007199254740991 || !activityCount(coverage.ExcludedIncompleteResults) || coverage.ExcludedIncompleteResults+hosted+cli > 10000 || len(coverage.Limits) < 1 || len(coverage.Limits) > 8 {
		return false
	}
	for _, limit := range coverage.Limits {
		if !ownerText(limit, 512, false) {
			return false
		}
	}
	return true
}
