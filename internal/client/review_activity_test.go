package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewActivityClientLimitDiagnosticAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		limit  bool
	}{{409, "review_activity_limit", true}, {401, "review_activity_limit", false}, {403, "review_activity_limit", false}, {409, "other_conflict", false}} {
		t.Run(tc.code+http.StatusText(tc.status), func(t *testing.T) {
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads.Add(1)
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/reviews/activity") || r.Header.Get("Idempotency-Key") != "" {
					t.Error("activity refusal retried or became a mutation")
				}
				write(w, tc.status, map[string]any{"error": map[string]any{"code": tc.code, "message": "Arbitrary private server detail"}, "request_id": "request-activity-limit"})
			}))
			defer server.Close()
			_, err := newClient(t, server).ReviewActivity(t.Context(), client.ReviewActivityOptions{Hours: 1})
			var diagnostic *client.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != tc.code || diagnostic.Status != tc.status || diagnostic.RequestID != "request-activity-limit" || diagnostic.OutcomeUnknown || reads.Load() != 1 {
				t.Fatal("error facts or single-read semantics changed", diagnostic, reads.Load())
			}
			if strings.Contains(diagnostic.Message+diagnostic.Action, "Arbitrary private") {
				t.Fatal("raw server text escaped", diagnostic)
			}
			if tc.limit {
				if !strings.Contains(diagnostic.Action, "Narrow the time window") || !strings.Contains(diagnostic.Action, "--pr") || strings.Contains(diagnostic.Action, "access") || strings.Contains(diagnostic.Action, "instance") {
					t.Fatal("activity limit lost actionable scope guidance", diagnostic)
				}
			} else if strings.Contains(diagnostic.Action, "Narrow the time window") {
				t.Fatal("activity hint rewrote authorization/unrelated refusal", diagnostic)
			}
		})
	}
}

func activityClientSample() (client.ReviewActivity, client.ReviewActivityOptions) {
	o := client.ReviewActivityOptions{Since: "2025-10-26T00:30:00Z", Until: "2025-10-26T02:30:00Z", Timezone: "Europe/Paris", PullNumber: 7}
	v := client.ReviewActivity{ProjectID: "project-one", AsOf: time.Now().UTC().Format(time.RFC3339Nano), Since: o.Since, Until: o.Until, Timezone: o.Timezone, PullNumber: 7, Totals: client.ReviewActivityCounts{Hosted: 1, CLI: 2}, AveragesPerHour: client.ReviewActivityAverages{Hosted: 0.5, CLI: 1}, Buckets: []client.ReviewActivityBucket{
		{Since: o.Since, Until: "2025-10-26T01:00:00Z", Label: "2025-10-26T02:00:00+02:00", Partial: true, Hosted: 1},
		{Since: "2025-10-26T01:00:00Z", Until: "2025-10-26T02:00:00Z", Label: "2025-10-26T02:00:00+01:00", CLI: 1},
		{Since: "2025-10-26T02:00:00Z", Until: o.Until, Label: "2025-10-26T03:00:00+01:00", Partial: true, CLI: 1},
	}, Diagnostics: client.ReviewActivityDiagnostics{Failed: 1}, Coverage: client.ReviewActivityCoverage{Basis: "current_retained_results", Limits: []string{"Retained source only."}, UndatedResults: 20000}}
	return v, o
}

func TestReviewActivityClientOneReadStrictWindowAndProjection(t *testing.T) {
	var response client.ReviewActivity
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		reads.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/activity" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.URL.Query().Get("pull_number") != "7" || r.URL.Query().Get("timezone") != "Europe/Paris" || r.URL.Query().Get("hours") != "" {
			t.Error("activity gained mutation, extra route or lost query/binding")
		}
		write(w, 200, response)
	}))
	defer server.Close()
	c := newClient(t, server)
	v, options := activityClientSample()
	response = v
	got, err := c.ReviewActivity(t.Context(), options)
	if err != nil || !reflect.DeepEqual(got, v) || reads.Load() != 1 {
		t.Fatal(got, err, reads.Load())
	}
	mutations := []struct {
		name   string
		change func(*client.ReviewActivity)
	}{
		{"project", func(v *client.ReviewActivity) { v.ProjectID = "other-project" }},
		{"pull", func(v *client.ReviewActivity) { v.PullNumber = 8 }},
		{"zone", func(v *client.ReviewActivity) { v.Timezone = "UTC" }},
		{"window", func(v *client.ReviewActivity) { v.Since = "2025-10-26T00:00:00Z" }},
		{"future-clock", func(v *client.ReviewActivity) { v.AsOf = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano) }},
		{"bucket-gap", func(v *client.ReviewActivity) { v.Buckets[1].Since = "2025-10-26T01:01:00Z" }},
		{"wrong-dst-offset", func(v *client.ReviewActivity) { v.Buckets[1].Label = v.Buckets[0].Label }},
		{"wrong-partial", func(v *client.ReviewActivity) { v.Buckets[0].Partial = false }},
		{"negative", func(v *client.ReviewActivity) { v.Buckets[0].Hosted = -1 }},
		{"totals", func(v *client.ReviewActivity) { v.Totals.CLI++ }},
		{"average", func(v *client.ReviewActivity) { v.AveragesPerHour.Hosted = 1 }},
		{"too-many-results", func(v *client.ReviewActivity) { v.Totals.Hosted = 10001 }},
		{"diagnostic", func(v *client.ReviewActivity) { v.Diagnostics.Held = -1 }},
		{"coverage-basis", func(v *client.ReviewActivity) { v.Coverage.Basis = "all_provider_reviews" }},
		{"coverage-count", func(v *client.ReviewActivity) { v.Coverage.UndatedResults = -1 }},
		{"missing-limits", func(v *client.ReviewActivity) { v.Coverage.Limits = nil }},
		{"unbounded-limits", func(v *client.ReviewActivity) { v.Coverage.Limits = make([]string, 9) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			// Deep-copy slices so each independent malformed response starts intact.
			body, _ := json.Marshal(v)
			_ = json.Unmarshal(body, &response)
			mutation.change(&response)
			_, err := c.ReviewActivity(t.Context(), options)
			checkError(t, err, "invalid_response")
		})
	}
	before := reads.Load()
	for _, options := range []client.ReviewActivityOptions{{Hours: -1}, {Hours: 169}, {Since: v.Since}, {Until: v.Until}, {Hours: 1, Since: v.Since, Until: v.Until}, {Timezone: "Local"}, {Timezone: "no_such_zone"}, {PullNumber: -1}, {Since: v.Until, Until: v.Since}, {Since: v.Since, Until: "2025-11-26T02:30:00Z"}} {
		if _, err := c.ReviewActivity(t.Context(), options); err == nil {
			t.Fatal("invalid local selector accepted", options)
		}
	}
	if reads.Load() != before {
		t.Fatal("invalid selectors made service requests")
	}
}
