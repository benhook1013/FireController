package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewActivityActualSDKAndCLIRetainedSourceRead(t *testing.T) {
	f := mutationFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	reviews.Register(f.handler.(*http.ServeMux), f.store)
	at := "2025-01-01T00:30:00Z"
	clean := reviews.RunInput{SourceRunID: "clean-hosted", Channel: "hosted", Scope: "Synthetic comparison", Outcome: "completed", Complete: true, Attributable: true, StartedAt: at, FinishedAt: at, Findings: []reviews.Finding{}}
	cliRun := clean
	cliRun.SourceRunID, cliRun.Channel = "clean-cli", "cli"
	failed := cliRun
	failed.SourceRunID, failed.Outcome, failed.Complete, failed.Attributable = "failed-empty", "failed", false, false
	zero := int64(0)
	input := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "activity-client-fixture", ObservedAt: "2025-01-01T01:00:00Z", Queue: []reviews.PullInput{{Number: 7, Title: "Retained activity", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: true, Runs: []reviews.RunInput{clean, cliRun, failed}, Attempts: []reviews.Attempt{{SourceAttemptID: "failed-attempt", Channel: "cli", State: "failed", StartedAt: at, FinishedAt: at}}}}}
	body, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("Idempotency-Key", core.NewID("seed"))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var events, receipts int
	if err := f.store.DB().QueryRow("SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)").Scan(&events, &receipts); err != nil {
		t.Fatal(err)
	}
	providerCalls := f.providers.Load()
	var activityReads, otherReads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/activity") {
			activityReads.Add(1)
			if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != f.sender {
				t.Error("activity not a bound provider-free read")
			}
		} else if r.URL.Path != "/api/v1/instance" {
			otherReads.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": identity}, f.credential)
	session := reviewControlsStdio(t, path, "chosen")
	var sdk client.ReviewActivity
	decodeResult(t, call(t, session, "review_activity_read", map[string]any{"since": "2025-01-01T00:00:00Z", "until": "2025-01-01T01:00:00Z", "timezone": "Pacific/Auckland", "pull_number": 7}), &sdk)
	if sdk.ProjectID != f.project || sdk.Totals != (client.ReviewActivityCounts{Hosted: 1, CLI: 1}) || sdk.Diagnostics.Failed != 1 || sdk.AveragesPerHour.Hosted != 1 || len(sdk.Buckets) != 1 || sdk.Buckets[0].Label != "2025-01-01T13:00:00+13:00" {
		t.Fatal(sdk)
	}
	var out, diag bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "reviews", "activity", "--since", "2025-01-01T00:00:00Z", "--until", "2025-01-01T01:00:00Z", "--timezone", "Pacific/Auckland", "--pr", "7"}, nil, &out, &diag)
	var command client.ReviewActivity
	if code != 0 || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &command) != nil || command.Totals != sdk.Totals || command.Diagnostics != sdk.Diagnostics || command.PullNumber != 7 {
		t.Fatal(code, out.String(), diag.String())
	}
	var defaults client.ReviewActivity
	decodeResult(t, call(t, session, "review_activity_read", nil), &defaults)
	if defaults.Timezone != "UTC" || defaults.PullNumber != 0 || defaults.Totals != (client.ReviewActivityCounts{}) || defaults.Diagnostics != (client.ReviewActivityDiagnostics{}) {
		t.Fatal("default window substituted snapshot date for original finish", defaults)
	}
	for _, raw := range []string{`{"hours":null}`, `{"hours":0}`, `{"timezone":""}`, `{"since":"2025-01-01T00:00:00Z"}`, `{"hours":1,"since":"2025-01-01T00:00:00Z","until":"2025-01-01T01:00:00Z"}`, `{"private":true}`} {
		diagnosticResult(t, call(t, session, "review_activity_read", json.RawMessage(raw)), "invalid_arguments", false)
	}
	var afterEvents, afterReceipts int
	if err := f.store.DB().QueryRow("SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)").Scan(&afterEvents, &afterReceipts); err != nil {
		t.Fatal(err)
	}
	if activityReads.Load() != 3 || otherReads.Load() != 0 || f.providers.Load() != providerCalls || events != afterEvents || receipts != afterReceipts {
		t.Fatal("activity made extra service/provider calls or mutations", activityReads.Load(), otherReads.Load(), events, afterEvents, receipts, afterReceipts)
	}
}
