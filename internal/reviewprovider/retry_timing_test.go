package reviewprovider

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRetryTimingExactFloor(t *testing.T) {
	now := time.Date(2026, 11, 9, 1, 2, 3, 750000000, time.UTC)
	date := now.Add(10 * time.Second).Truncate(time.Second)
	tests := []struct {
		name    string
		headers http.Header
		state   string
		due     time.Time
	}{
		{"seconds", http.Header{"Retry-After": {"12"}}, "proven", now.Add(12 * time.Second)},
		{"zero", http.Header{"Retry-After": {"0"}}, "proven", now},
		{"date fractional second preserved", http.Header{"Retry-After": {date.Format(http.TimeFormat)}}, "proven", date},
		{"past date", http.Header{"Retry-After": {now.Add(-time.Hour).Format(http.TimeFormat)}}, "proven", now.Add(-time.Hour).Truncate(time.Second)},
		{"maximum", http.Header{"Retry-After": {"86400"}}, "proven", now.Add(24 * time.Hour)},
		{"capped legacy is not proof", http.Header{"Retry-After": {"90000"}}, "unsupported", time.Time{}},
		{"overflow", http.Header{"Retry-After": {"999999999999999999999999"}}, "unsupported", time.Time{}},
		{"far date", http.Header{"Retry-After": {now.Add(48 * time.Hour).Format(http.TimeFormat)}}, "unsupported", time.Time{}},
		{"absent", http.Header{}, "unknown", time.Time{}},
		{"blank", http.Header{"Retry-After": {" "}}, "unknown", time.Time{}},
		{"negative", http.Header{"Retry-After": {"-1"}}, "unknown", time.Time{}},
		{"malformed", http.Header{"Retry-After": {"private token"}}, "unknown", time.Time{}},
		{"duplicate", http.Header{"Retry-After": {"1", "20"}}, "unknown", time.Time{}},
		{"joined duplicate", http.Header{"Retry-After": {"1, 20"}}, "unknown", time.Time{}},
		{"reset", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(date.Unix(), 10)}}, "proven", date},
		{"later reset wins", http.Header{"Retry-After": {"1"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(date.Unix(), 10)}}, "proven", date},
		{"later relative wins", http.Header{"Retry-After": {"12"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(date.Unix(), 10)}}, "proven", now.Add(12 * time.Second)},
		{"reset unsupported", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10)}}, "unsupported", time.Time{}},
		{"reset incomplete", http.Header{"Retry-After": {"12"}, "X-Ratelimit-Remaining": {"0"}}, "unknown", time.Time{}},
		{"reset duplicate", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1", "2"}}, "unknown", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := retryTiming(tc.headers, now)
			if got.State != tc.state || !got.NotBefore.Equal(tc.due) || !got.ObservedAt.Equal(now) {
				t.Fatalf("got %+v; want %s %s", got, tc.state, tc.due)
			}
		})
	}
	// Demonstrate the old hint is unchanged and cannot be used as a floor.
	if retryAfter(http.Header{"Retry-After": {"90000"}}, now) != 86400 || retryAfter(http.Header{"Retry-After": {date.Format(http.TimeFormat)}}, now) != 9 {
		t.Fatal("legacy hint changed")
	}
}

func TestStatusErrorRetainsTimingWithoutChangingSafety(t *testing.T) {
	for _, status := range []int{429, 403, 401, 500} {
		for _, mutation := range []bool{false, true} {
			before := time.Now()
			err := statusError(&http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"12"}}}, mutation)
			after := time.Now()
			var failure *Error
			if !errors.As(err, &failure) || failure.RetryAfterSeconds != 12 || failure.RetryTiming.State != "proven" {
				t.Fatal(err)
			}
			timing := failure.RetryTiming
			if timing.ObservedAt.Before(before) || timing.ObservedAt.After(after) || !timing.NotBefore.Equal(timing.ObservedAt.Add(12*time.Second)) {
				t.Fatal(timing)
			}
			if failure.OutcomeUnknown != (mutation && status == 500) {
				t.Fatal("mutation uncertainty changed")
			}
			if status == 401 && failure.Code != "provider_unauthorized" {
				t.Fatal("timing changed authorization classification")
			}
		}
	}
}
