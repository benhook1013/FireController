package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewToolsReadNativeSnapshotsWithoutEffects(t *testing.T) {
	f := mutationFixture(t)
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	var writes, reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.Contains(r.URL.Path, "/reviews/") {
			reads.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var empty client.ReviewQueue
	decodeResult(t, call(t, session, "review_queue", map[string]any{}), &empty)
	if empty.Configured || empty.Revision != 0 || len(empty.Items) != 0 {
		t.Fatal("missing observation not explicit")
	}
	zero := int64(0)
	observation := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "synthetic MCP fixture", ObservedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)}
	for i := 1; i <= 6; i++ {
		pull := reviews.PullInput{Number: int64(i), Title: "Stored PR " + strconv.Itoa(i), State: "open", HeadSHA: strings.Repeat("a", 40), CIState: "unknown", Mergeability: "unknown"}
		if i == 1 {
			pull.State = "merged"
			for j := 0; j < 6; j++ {
				run := reviews.RunInput{SourceRunID: "source-run-" + strconv.Itoa(j), Channel: "subagent", Scope: "historical scope", Outcome: "unknown", Found: 1, CoverageLimits: []string{"Retained partial source evidence."}}
				if j == 5 {
					run.Findings = []reviews.Finding{{SourceFindingID: "finding", Title: "Explicit detail", Detail: "PRIVATE_FINDING_DETAIL", Severity: "unknown", Disposition: "accepted", DecisionReason: "Reproduced", ProofNote: "Focused source proof"}}
					run.Accepted = 1
				}
				pull.Runs = append(pull.Runs, run)
			}
			for k := 0; k < 6; k++ {
				pull.Attempts = append(pull.Attempts, reviews.Attempt{SourceAttemptID: "attempt-" + strconv.Itoa(k), Channel: "subagent", State: "unknown", SourceRunID: "source-run-5"})
			}
		}
		observation.Queue = append(observation.Queue, pull)
	}
	data, _ := json.Marshal(observation)
	r := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("Idempotency-Key", core.NewID("fixture"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("synthetic observation HTTP %d: %s", w.Code, w.Body.String())
	}
	counts := func() [2]int {
		t.Helper()
		var out [2]int
		for i, table := range []string{"events", "receipts"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before, providerBefore := counts(), f.providers.Load()
	var queue, tail client.ReviewQueue
	result := call(t, session, "review_queue", map[string]any{})
	decodeResult(t, result, &queue)
	compact, _ := json.Marshal(result.StructuredContent)
	if len(queue.Items) != 5 || !queue.HasMore || queue.Items[0].State != "merged" || strings.Contains(string(compact), "PRIVATE_FINDING_DETAIL") {
		t.Fatal("compact default-five queue or retained merged history changed")
	}
	decodeResult(t, call(t, session, "review_queue", map[string]any{"cursor": queue.Cursor}), &tail)
	if len(tail.Items) != 1 || tail.Items[0].Number != 6 || tail.HasMore || tail.SnapshotID != queue.SnapshotID {
		t.Fatal("queue continuation lost")
	}
	var pull client.ReviewPull
	decodeResult(t, call(t, session, "review_pull", map[string]any{"pull_number": 1, "snapshot": queue.SnapshotID}), &pull)
	if pull.Pull.Complete || pull.Pull.AttemptCount != 6 {
		t.Fatal("incomplete observation or unknown duration repaired")
	}
	var attempts, attemptTail client.ReviewAttempts
	decodeResult(t, call(t, session, "review_attempts", map[string]any{"pull_number": 1, "snapshot": queue.SnapshotID}), &attempts)
	if len(attempts.Items) != 5 || !attempts.HasMore || attempts.Items[0].SourceAttemptID != "attempt-5" || attempts.Items[0].DurationSeconds != nil || attempts.Items[0].Source != "synthetic MCP fixture" {
		t.Fatal("attempt default-five, order, source or unknown duration changed")
	}
	decodeResult(t, call(t, session, "review_attempts", map[string]any{"pull_number": 1, "snapshot": queue.SnapshotID, "cursor": attempts.Cursor}), &attemptTail)
	if len(attemptTail.Items) != 1 || attemptTail.HasMore || attemptTail.SnapshotID != queue.SnapshotID || attemptTail.Items[0].SourceAttemptID != "attempt-0" {
		t.Fatal("explicit attempt continuation lost an item")
	}
	var runs, last client.ReviewRuns
	decodeResult(t, call(t, session, "review_runs", map[string]any{"pull_number": 1, "snapshot": queue.SnapshotID}), &runs)
	if len(runs.Items) != 5 || !runs.HasMore || runs.Items[0].SourceRunID != "source-run-5" {
		t.Fatal("run default-five producer ordering lost")
	}
	decodeResult(t, call(t, session, "review_runs", map[string]any{"pull_number": 1, "snapshot": queue.SnapshotID, "cursor": runs.Cursor}), &last)
	if len(last.Items) != 1 || last.HasMore || last.Items[0].SourceRunID != "source-run-0" {
		t.Fatal("run continuation lost")
	}
	var detail client.ReviewRunDetail
	decodeResult(t, call(t, session, "review_run", map[string]any{"run_id": runs.Items[0].ID, "snapshot": queue.SnapshotID}), &detail)
	if detail.SnapshotID != queue.SnapshotID || detail.Run.Complete || len(detail.Findings) != 1 || detail.Findings[0].Detail != "PRIVATE_FINDING_DETAIL" || len(detail.Run.CoverageLimits) != 1 {
		t.Fatal("explicit source evidence changed")
	}
	if writes.Load() != 0 || reads.Load() != 9 || counts() != before || f.providers.Load() != providerBefore {
		t.Fatal("review tool performed extra requests, provider work or durable effects")
	}
}

func TestReviewToolArgumentsFailBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "absent-private-credential")
	session := connect(t, adapter(t, path, "alpha"), "")
	for i, test := range []struct{ name, raw string }{
		{"review_queue", `{"limit":0}`}, {"review_queue", `{"limit":101}`}, {"review_queue", `{"limit":1,"limit":2}`},
		{"review_queue", `{"cursor":null}`}, {"review_queue", `{"cursor":" padded "}`}, {"review_queue", `{"cursor":"bad\u0000"}`},
		{"review_queue", `{"cursor":"` + strings.Repeat("x", 4097) + `"}`}, {"review_queue", `{"snapshot":"other"}`},
		{"review_attempts", `{}`}, {"review_attempts", `{"pull_number":null}`}, {"review_attempts", `{"pull_number":0}`}, {"review_attempts", `{"pull_number":2147483648}`}, {"review_attempts", `{"pull_number":1,"limit":101}`}, {"review_attempts", `{"pull_number":1,"cursor":" bad"}`}, {"review_attempts", `{"pull_number":1,"snapshot":"bad"}`}, {"review_attempts", `{"pull_number":1,"project_id":"override"}`}, {"review_attempts", `{"pull_number":1,"limit":null}`},
		{"review_pull", `{}`}, {"review_pull", `{"pull_number":null}`}, {"review_pull", `{"pull_number":0}`},
		{"review_pull", `{"pull_number":2147483648}`}, {"review_pull", `{"pull_number":1.5}`}, {"review_pull", `{"pull_number":"1"}`},
		{"review_pull", `{"Pull_number":1}`}, {"review_pull", `{"pull_number":1,"pull_number":2}`},
		{"review_runs", `{"pull_number":1,"snapshot":""}`}, {"review_runs", `{"pull_number":1,"snapshot":null}`},
		{"review_runs", `{"pull_number":1,"snapshot":"review_snapshot_` + strings.Repeat("A", 32) + `"}`},
		{"review_runs", `{"pull_number":1,"project_id":"override"}`},
		{"review_run", `{"run_id":"../other"}`}, {"review_run", `{"run_id":"review_run_` + strings.Repeat("a", 32) + `","limit":1}`},
		{"review_run", `{"run_id":"bad\ud800"}`}, {"review_run", `{"run_id":null}`},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			diagnosticResult(t, call(t, session, test.name, json.RawMessage(test.raw)), "invalid_arguments", false)
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid review arguments reached HTTP")
	}
}
