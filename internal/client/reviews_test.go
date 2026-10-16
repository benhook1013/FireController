package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

type reviewClientFixture struct {
	store   *core.Store
	server  *httptest.Server
	client  *client.Client
	admin   string
	project string
	calls   atomic.Int64
}

func newReviewClientFixture(t *testing.T) *reviewClientFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := domain.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('project-one','Native review test',701,'owner/repository')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES('project-one','worker-one','Builder')`); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testToken))
	if _, err := store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES('review-client-credential','review-client-principal',?,0,0)`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('review-client-credential','project-one',1,0)`); err != nil {
		t.Fatal(err)
	}
	adminData, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &reviewClientFixture{store: store, admin: strings.TrimSpace(string(adminData)), project: "project-one"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/instance", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Worker") != "" {
			t.Error("instance handshake was not bound correctly")
		}
		write(w, http.StatusOK, map[string]string{"instance_id": store.InstanceID()})
	})
	domain.Register(mux, store)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		mux.ServeHTTP(w, r)
	}))
	ctx := profileBinding(t, f.server.URL, store.InstanceID(), f.project)
	f.client, err = client.New(context.Background(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.client.Close(); f.server.Close() })
	return f
}

func profileBinding(t *testing.T, serverURL, instance, project string) profile.Binding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private.token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return profile.Binding{Context: profile.Context{Profile: "review-test", ServerURL: serverURL, InstanceID: instance, ProjectID: project, WorkerID: "worker-one"}, CredentialFile: path}
}

func (f *reviewClientFixture) importSnapshot(t *testing.T, adjust ...func(*domain.ObservationRequest)) string {
	t.Helper()
	revision := int64(0)
	started := "2026-10-15T12:00:00Z"
	finished := "2026-10-15T12:01:00Z"
	req := domain.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "explicit-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []domain.PullInput{{
		Number: 1, Title: "Retained PR", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: true,
		Runs:     []domain.RunInput{{SourceRunID: "run-source-1", Channel: "manual", HeadSHA: strings.Repeat("a", 40), Reviewer: "operator", Scope: "focused", Outcome: "completed", Attributable: true, Complete: true, StartedAt: started, FinishedAt: finished, Found: 1, Accepted: 1, Finalized: true, CoverageLimits: []string{"stored source only"}, Findings: []domain.Finding{{SourceFindingID: "finding-1", Title: "Finding title", Detail: "A detailed source finding", Severity: "high", Disposition: "accepted", DecisionReason: "Evidence supports it", Actor: "operator", DecidedAt: finished, FixSHA: strings.Repeat("c", 40), ProofNote: "Verified", ResolvedAt: finished}}}},
		Attempts: []domain.Attempt{{SourceAttemptID: "attempt-unknown-duration", Channel: "manual", HeadSHA: strings.Repeat("a", 40), State: "unknown", SourceRunID: "run-source-1"}, {SourceAttemptID: "attempt-zero-duration", Channel: "subagent", HeadSHA: strings.Repeat("a", 40), State: "completed", StartedAt: started, FinishedAt: finished, DurationSeconds: int64Ptr(0), SourceRunID: "run-source-1"}},
	}}}
	for _, change := range adjust {
		change(&req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("Idempotency-Key", "client-review-import")
	w := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed review import: %d %s", w.Code, w.Body.String())
	}
	var receipt domain.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.ID == "" {
		t.Fatalf("seed receipt: %v %s", err, w.Body.String())
	}
	return receipt.ID
}

func int64Ptr(value int64) *int64 { return &value }

func TestReviewReadsUseScopedNativeProjectionAndPreserveNullableDuration(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t)
	var beforeEvents, beforeReceipts int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&beforeReceipts); err != nil {
		t.Fatal(err)
	}
	if f.client.Identity().ProjectID != f.project {
		t.Fatal("client identity changed")
	}
	queue, err := f.client.ReviewQueue(context.Background(), client.PageOptions{Limit: 5})
	if err != nil || !queue.Configured || queue.ProjectID != f.project || queue.SnapshotID != snapshot || queue.Revision != 1 || len(queue.Items) != 1 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
	pull, err := f.client.ReviewPull(context.Background(), 1, snapshot)
	if err != nil || pull.Pull.Number != 1 || len(pull.Attempts) != 2 {
		t.Fatalf("pull=%+v err=%v", pull, err)
	}
	if pull.Attempts[0].DurationSeconds != nil || pull.Attempts[1].DurationSeconds == nil || *pull.Attempts[1].DurationSeconds != 0 {
		t.Fatalf("nullable duration collapsed: %+v", pull.Attempts)
	}
	runs, err := f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{PageOptions: client.PageOptions{Limit: 5}, Snapshot: snapshot})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].CoverageLimits[0] != "stored source only" || runs.Items[0].Source != "explicit-import" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	detail, err := f.client.ReviewRun(context.Background(), runs.Items[0].ID, snapshot)
	if err != nil || len(detail.Findings) != 1 || detail.Findings[0].Detail != "A detailed source finding" || detail.Run.Fixed != 1 || detail.Run.Source != "explicit-import" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if f.calls.Load() != 6 {
		t.Fatalf("unexpected handshake/provider/retry calls: %d", f.calls.Load())
	}
	var afterEvents, afterReceipts int
	_ = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&afterEvents)
	_ = f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&afterReceipts)
	if afterEvents != beforeEvents || afterReceipts != beforeReceipts {
		t.Fatal("review reads changed service state")
	}
	encoded, _ := json.Marshal(detail)
	if strings.Contains(string(encoded), f.admin) || strings.Contains(string(encoded), testToken) {
		t.Fatal("typed review response exposed credentials")
	}
}

func TestReviewSelectorsRejectBeforeHTTPAndBindExplicitSnapshot(t *testing.T) {
	f := newReviewClientFixture(t)
	before := f.calls.Load()
	invalid := []func() error{
		func() error { _, err := f.client.ReviewPull(context.Background(), 0, ""); return err },
		func() error { _, err := f.client.ReviewPull(context.Background(), 2147483648, ""); return err },
		func() error {
			_, err := f.client.ReviewPull(context.Background(), 1, "review_snapshot_BAD")
			return err
		},
		func() error {
			_, err := f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{PageOptions: client.PageOptions{Limit: 101}})
			return err
		},
		func() error {
			_, err := f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{PageOptions: client.PageOptions{Cursor: " bad"}})
			return err
		},
		func() error { _, err := f.client.ReviewRun(context.Background(), "review_run_short", ""); return err },
	}
	for _, call := range invalid {
		err := call()
		checkError(t, err, "validation_failed")
	}
	if f.calls.Load() != before {
		t.Fatal("invalid selectors made service requests")
	}
	snapshot := f.importSnapshot(t)
	wrong := "review_snapshot_" + strings.Repeat("f", 32)
	if _, err := f.client.ReviewPull(context.Background(), 1, wrong); err == nil {
		t.Fatal("server response escaped the explicitly selected snapshot")
	}
	checkError(t, func() error {
		_, err := f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{Snapshot: wrong})
		return err
	}(), "not_found")
	if _, err := f.client.ReviewRun(context.Background(), "review_run_"+strings.Repeat("0", 32), snapshot); err == nil {
		t.Fatal("unavailable run detail accepted")
	}
}

func TestReviewClientRejectsMalformedNullableAndUnscopedResponses(t *testing.T) {
	validPull := client.ReviewPullSummary{Number: 1, Title: "PR", State: "merged", BaseRef: "main", CIState: "unknown", Mergeability: "unknown", RunCount: 0, AttemptCount: 0, RecentRuns: []client.ReviewRunSummary{}, LatestAttempts: []client.ReviewAttempt{}}
	meta := map[string]any{"project_id": "project-one", "revision": 1, "snapshot_id": "review_snapshot_" + strings.Repeat("b", 32), "source": "import", "observed_at": "2026-10-15T12:00:00Z", "recorded_at": "2026-10-15T12:01:00Z"}
	queueBody := func(pull client.ReviewPullSummary) string {
		value := map[string]any{}
		for k, v := range meta {
			value[k] = v
		}
		value["configured"] = true
		value["items"] = []client.ReviewPullSummary{pull}
		value["has_more"] = false
		data, _ := json.Marshal(value)
		return string(data)
	}
	bodies := []struct{ name, body string }{
		{"wrong_project", strings.Replace(queueBody(validPull), `"project-one"`, `"other-project"`, 1)},
		{"null_items", strings.Replace(queueBody(validPull), `"items":[`, `"items":null`, 1)},
		{"negative_count", strings.Replace(queueBody(validPull), `"attempt_count":0`, `"attempt_count":-1`, 1)},
		{"malformed_snapshot", strings.Replace(queueBody(validPull), meta["snapshot_id"].(string), "bad-snapshot", 1)},
		{"unknown_member", strings.Replace(queueBody(validPull), `"configured":true`, `"configured":true,"merge_ready":true`, 1)},
		{"duplicate_member", strings.Replace(queueBody(validPull), `"revision":1`, `"revision":1,"revision":1`, 1)},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewMock(t, tc.body, "/api/v1/projects/project-one/reviews/queue")
			_, err := f.client.ReviewQueue(context.Background(), client.PageOptions{})
			checkError(t, err, "invalid_response")
		})
	}

	validPull.AttemptCount = 1
	validPull.LatestAttempts = []client.ReviewAttempt{{SourceAttemptID: "attempt", Channel: "manual", State: "unknown"}}
	validResponse := client.ReviewPull{ProjectID: "project-one", Revision: 1, SnapshotID: meta["snapshot_id"].(string), Source: "import", ObservedAt: meta["observed_at"].(string), RecordedAt: meta["recorded_at"].(string), Pull: validPull, Attempts: []client.ReviewAttempt{{SourceAttemptID: "attempt", Channel: "manual", State: "unknown"}}}
	validPullBody, _ := json.Marshal(validResponse)
	validMock := newReviewMock(t, string(validPullBody), "/api/v1/projects/project-one/reviews/pulls/1")
	result, err := validMock.client.ReviewPull(context.Background(), 1, "")
	if err != nil || len(result.Attempts) != 1 || result.Attempts[0].DurationSeconds != nil {
		t.Fatalf("nullable duration should remain unknown: %+v %v", result, err)
	}
	for _, tc := range []struct{ name, body string }{
		{"missing_duration", strings.Replace(string(validPullBody), `,"duration_seconds":null`, "", 1)},
		{"negative_duration", strings.Replace(string(validPullBody), `"duration_seconds":null`, `"duration_seconds":-1`, 1)},
		{"duration_string", strings.Replace(string(validPullBody), `"duration_seconds":null`, `"duration_seconds":"unknown"`, 1)},
		{"nullable_title", strings.Replace(string(validPullBody), `"title":"PR"`, `"title":null`, 1)},
		{"truncated_attempts", strings.Replace(string(validPullBody), `"attempt_count":1`, `"attempt_count":2`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newReviewMock(t, tc.body, "/api/v1/projects/project-one/reviews/pulls/1")
			_, err := mock.client.ReviewPull(context.Background(), 1, "")
			checkError(t, err, "invalid_response")
		})
	}
	if _, err := validMock.client.ReviewPull(context.Background(), 1, "review_snapshot_"+strings.Repeat("c", 32)); err == nil {
		t.Fatal("response snapshot mismatch accepted")
	}
	oversized := newReviewMock(t, strings.Repeat(" ", (1<<20)+1), "/api/v1/projects/project-one/reviews/queue")
	_, err = oversized.client.ReviewQueue(context.Background(), client.PageOptions{})
	checkError(t, err, "invalid_response")
}

type reviewMock struct {
	server *httptest.Server
	client *client.Client
}

func newReviewMock(t *testing.T, body, path string) *reviewMock {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/instance", func(w http.ResponseWriter, r *http.Request) { handshake(w) })
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	s := httptest.NewServer(mux)
	c, err := client.New(context.Background(), binding(t, s.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return &reviewMock{s, c}
}

func TestReviewReadsDefaultToCurrentSnapshotAndKeepHistoricalCounts(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t, func(req *domain.ObservationRequest) {
		// Finalized source counts and effective later finding dispositions are
		// distinct observations; the client must not recompute source credit.
		req.Queue[0].Runs[0].Accepted = 0
	})
	pull, err := f.client.ReviewPull(context.Background(), 1, "")
	if err != nil || pull.SnapshotID != snapshot || len(pull.Attempts) != 2 {
		t.Fatalf("current pull default failed: %+v %v", pull, err)
	}
	runs, err := f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{})
	if err != nil || runs.SnapshotID != snapshot || len(runs.Items) != 1 {
		t.Fatalf("current runs default failed: %+v %v", runs, err)
	}
	full, err := f.client.ReviewRun(context.Background(), runs.Items[0].ID, "")
	if err != nil || full.SnapshotID != snapshot || full.Run.Accepted != 0 || full.Run.Fixed != 1 {
		t.Fatalf("source counts/effective fix changed: %+v %v", full, err)
	}
	if f.calls.Load() != 5 {
		t.Fatalf("unexpected provider/handshake/retry requests: %d", f.calls.Load())
	}
}

func TestReviewAttemptProgressStatesSurviveStoredQueueAndPullReads(t *testing.T) {
	f := newReviewClientFixture(t)
	states := []string{"preparing", "held", "prepared"}
	f.importSnapshot(t, func(req *domain.ObservationRequest) {
		req.Queue[0].Attempts = nil
		for i, state := range states {
			req.Queue[0].Attempts = append(req.Queue[0].Attempts, domain.Attempt{SourceAttemptID: "attempt-" + state, Channel: []string{"cli", "hosted", "manual"}[i], HeadSHA: strings.Repeat("a", 40), State: state, StartedAt: "2026-10-15T12:00:00Z"})
		}
	})
	queue, err := f.client.ReviewQueue(context.Background(), client.PageOptions{})
	if err != nil || len(queue.Items) != 1 || len(queue.Items[0].LatestAttempts) != len(states) {
		t.Fatalf("queue progress states missing: %+v %v", queue, err)
	}
	pull, err := f.client.ReviewPull(context.Background(), 1, "")
	if err != nil || len(pull.Attempts) != len(states) {
		t.Fatalf("pull progress states missing: %+v %v", pull, err)
	}
	queueStates := make(map[string]string)
	for _, attempt := range queue.Items[0].LatestAttempts {
		queueStates[attempt.SourceAttemptID] = attempt.State
	}
	for i, state := range states {
		if queueStates["attempt-"+state] != state || pull.Attempts[i].State != state {
			t.Fatal("native progress state was normalized or legacy prepared was lost")
		}
	}
}

func TestReviewProgressStatesDoNotWeakenAttemptOrRunValidation(t *testing.T) {
	snapshot := "review_snapshot_" + strings.Repeat("b", 32)
	timestamp := "2026-10-15T12:00:00Z"
	meta := map[string]any{"project_id": "project-one", "revision": 1, "snapshot_id": snapshot, "source": "native-executor", "observed_at": timestamp, "recorded_at": timestamp}
	attempt := client.ReviewAttempt{SourceAttemptID: "attempt", Channel: "cli", State: "prepared"}
	run := client.ReviewRunSummary{Source: "native-executor", ID: "review_run_" + strings.Repeat("a", 32), PullNumber: 1, SourceRunID: "run", Channel: "cli", Scope: "stored", Outcome: "unknown", CoverageLimits: []string{}}
	invoke := func(kind, state, outcome string) error {
		t.Helper()
		currentAttempt, currentRun := attempt, run
		currentAttempt.State, currentRun.Outcome = state, outcome
		pull := client.ReviewPullSummary{Number: 1, Title: "PR", State: "open", CIState: "unknown", Mergeability: "unknown", RunCount: 1, AttemptCount: 1, RecentRuns: []client.ReviewRunSummary{currentRun}, LatestAttempts: []client.ReviewAttempt{currentAttempt}}
		body := map[string]any{}
		for key, value := range meta {
			body[key] = value
		}
		path := "/api/v1/projects/project-one/reviews"
		switch kind {
		case "queue":
			path += "/queue"
			body["configured"], body["items"], body["has_more"] = true, []client.ReviewPullSummary{pull}, false
		case "pull":
			path += "/pulls/1"
			body["pull"], body["attempts"] = pull, []client.ReviewAttempt{currentAttempt}
		case "runs":
			path += "/pulls/1/runs"
			body["items"], body["has_more"] = []client.ReviewRunSummary{currentRun}, false
		case "detail":
			path += "/runs/" + run.ID
			body["pull_number"], body["run"], body["findings"] = 1, currentRun, []client.ReviewFinding{}
		}
		encoded, _ := json.Marshal(body)
		f := newReviewMock(t, string(encoded), path)
		var err error
		switch kind {
		case "queue":
			_, err = f.client.ReviewQueue(context.Background(), client.PageOptions{})
		case "pull":
			_, err = f.client.ReviewPull(context.Background(), 1, "")
		case "runs":
			_, err = f.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{})
		case "detail":
			_, err = f.client.ReviewRun(context.Background(), run.ID, "")
		}
		return err
	}
	for _, state := range []string{"queued", "HELD", "preparing ", "", "hold"} {
		for _, kind := range []string{"queue", "pull"} {
			t.Run(kind+"/invalid-attempt/"+state, func(t *testing.T) {
				checkError(t, invoke(kind, state, "unknown"), "invalid_response")
			})
		}
	}
	for _, outcome := range []string{"preparing", "held"} {
		for _, kind := range []string{"queue", "pull", "runs", "detail"} {
			t.Run(kind+"/attempt-only-outcome/"+outcome, func(t *testing.T) {
				checkError(t, invoke(kind, "prepared", outcome), "invalid_response")
			})
		}
	}
}

func TestReviewClientRequiresRunOriginAndKeepsMixedProducerIdentity(t *testing.T) {
	run := client.ReviewRunSummary{Source: " original producer ", ID: "review_run_" + strings.Repeat("a", 32), PullNumber: 1, SourceRunID: "same-external-id", Channel: "manual", Scope: "stored", Outcome: "unknown", CoverageLimits: []string{}}
	meta := map[string]any{"project_id": "project-one", "revision": 1, "snapshot_id": "review_snapshot_" + strings.Repeat("b", 32), "source": "native snapshot publisher", "observed_at": "2026-10-15T12:00:00Z", "recorded_at": "2026-10-15T12:01:00Z"}
	makeBody := func(kind string, items []client.ReviewRunSummary) map[string]any {
		value := map[string]any{}
		for key, item := range meta {
			value[key] = item
		}
		pull := client.ReviewPullSummary{Number: 1, Title: "PR", State: "open", CIState: "unknown", Mergeability: "unknown", RunCount: int64(len(items)), RecentRuns: items, LatestAttempts: []client.ReviewAttempt{}}
		switch kind {
		case "queue":
			value["configured"], value["items"], value["has_more"] = true, []client.ReviewPullSummary{pull}, false
		case "pull":
			value["pull"], value["attempts"] = pull, []client.ReviewAttempt{}
		case "runs":
			value["items"], value["has_more"] = items, false
		case "detail":
			value["pull_number"], value["run"], value["findings"] = 1, items[0], []client.ReviewFinding{}
		}
		return value
	}
	call := func(kind string, body []byte) (any, error) {
		t.Helper()
		paths := map[string]string{"queue": "/queue", "pull": "/pulls/1", "runs": "/pulls/1/runs", "detail": "/runs/" + run.ID}
		mock := newReviewMock(t, string(body), "/api/v1/projects/project-one/reviews"+paths[kind])
		switch kind {
		case "queue":
			return mock.client.ReviewQueue(context.Background(), client.PageOptions{})
		case "pull":
			return mock.client.ReviewPull(context.Background(), 1, "")
		case "runs":
			return mock.client.ReviewRuns(context.Background(), 1, client.ReviewRunOptions{})
		default:
			return mock.client.ReviewRun(context.Background(), run.ID, "")
		}
	}
	for _, kind := range []string{"queue", "pull", "runs", "detail"} {
		body, _ := json.Marshal(makeBody(kind, []client.ReviewRunSummary{run}))
		value, err := call(kind, body)
		if err != nil {
			t.Fatal("valid original producer rejected", kind, err)
		}
		encoded, _ := json.Marshal(value)
		if !strings.Contains(string(encoded), `"source":" original producer "`) || !strings.Contains(string(encoded), `"source":"native snapshot publisher"`) {
			t.Fatal("original producer or snapshot publisher was relabeled")
		}
		for _, source := range []string{"", " \t\n", strings.Repeat("🔥", 33), "bad\x00source"} {
			invalid := run
			invalid.Source = source
			body, _ := json.Marshal(makeBody(kind, []client.ReviewRunSummary{invalid}))
			_, err := call(kind, body)
			checkError(t, err, "invalid_response")
		}
		missing := strings.Replace(string(body), `"source":" original producer ",`, "", 1)
		_, err = call(kind, []byte(missing))
		checkError(t, err, "invalid_response")
		nullSource := strings.Replace(string(body), `"source":" original producer "`, `"source":null`, 1)
		_, err = call(kind, []byte(nullSource))
		checkError(t, err, "invalid_response")
	}
	other := run
	other.Source, other.ID = "native producer", "review_run_"+strings.Repeat("c", 32)
	for _, kind := range []string{"queue", "pull", "runs"} {
		body, _ := json.Marshal(makeBody(kind, []client.ReviewRunSummary{run, other}))
		if _, err := call(kind, body); err != nil {
			t.Fatal("distinct producers sharing an external run ID were conflated", kind, err)
		}
		other.Source = run.Source
		body, _ = json.Marshal(makeBody(kind, []client.ReviewRunSummary{run, other}))
		_, err := call(kind, body)
		checkError(t, err, "invalid_response")
		other.Source = "native producer"
	}
}

func TestReviewClientRejectsBlankCoverageLimitsAcrossViews(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t)
	queue, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := f.client.ReviewPull(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.client.ReviewRun(t.Context(), runs.Items[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []string{"", " \t\r\n"} {
		for _, view := range []string{"queue", "pull", "runs", "run"} {
			t.Run(view+"-"+strconv.Itoa(len(limit)), func(t *testing.T) {
				var body any
				var path string
				switch view {
				case "queue":
					value := queue
					value.Items = append([]client.ReviewPullSummary(nil), queue.Items...)
					value.Items[0].RecentRuns = append([]client.ReviewRunSummary(nil), queue.Items[0].RecentRuns...)
					value.Items[0].RecentRuns[0].CoverageLimits = []string{limit}
					body = value
					path = "/api/v1/projects/project-one/reviews/queue"
				case "pull":
					value := pull
					value.Pull.RecentRuns = append([]client.ReviewRunSummary(nil), pull.Pull.RecentRuns...)
					value.Pull.RecentRuns[0].CoverageLimits = []string{limit}
					body = value
					path = "/api/v1/projects/project-one/reviews/pulls/1"
				case "runs":
					value := runs
					value.Items = append([]client.ReviewRunSummary(nil), runs.Items...)
					value.Items[0].CoverageLimits = []string{limit}
					body = value
					path = "/api/v1/projects/project-one/reviews/pulls/1/runs"
				case "run":
					value := detail
					value.Run.CoverageLimits = []string{limit}
					body = value
					path = "/api/v1/projects/project-one/reviews/runs/" + detail.Run.ID
				}
				data, _ := json.Marshal(body)
				mock := newReviewMock(t, string(data), path)
				switch view {
				case "queue":
					_, err = mock.client.ReviewQueue(t.Context(), client.PageOptions{})
				case "pull":
					_, err = mock.client.ReviewPull(t.Context(), 1, "")
				case "runs":
					_, err = mock.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
				case "run":
					_, err = mock.client.ReviewRun(t.Context(), detail.Run.ID, "")
				}
				checkError(t, err, "invalid_response")
			})
		}
	}
}

func TestReviewClientProofNotesRequireAcceptedDisposition(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t, func(req *domain.ObservationRequest) {
		finding := &req.Queue[0].Runs[0].Findings[0]
		finding.FixSHA = ""
		finding.ResolvedAt = ""
		finding.ProofNote = "Accepted proof before a known fix SHA"
		req.Queue[0].Runs[0].CoverageLimits = []string{"  Retained scope limitation\n"}
	})
	runs, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.client.ReviewRun(t.Context(), runs.Items[0].ID, "")
	if err != nil || detail.Findings[0].ProofNote != "Accepted proof before a known fix SHA" || detail.Run.CoverageLimits[0] != "  Retained scope limitation\n" {
		t.Fatal("accepted source text rejected or normalized", detail, err)
	}
	for _, disposition := range []string{"unadjudicated", "routed", "rejected"} {
		t.Run(disposition, func(t *testing.T) {
			value := detail
			value.Findings = append([]client.ReviewFinding(nil), detail.Findings...)
			value.Findings[0].Disposition = disposition
			data, _ := json.Marshal(value)
			mock := newReviewMock(t, string(data), "/api/v1/projects/project-one/reviews/runs/"+value.Run.ID)
			_, err := mock.client.ReviewRun(t.Context(), value.Run.ID, "")
			checkError(t, err, "invalid_response")
		})
	}
}

func TestReviewClientRejectsNoncanonicalSHAsInPublicResponses(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t)
	queue, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := f.client.ReviewPull(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.client.ReviewRun(t.Context(), runs.Items[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var beforeEvents, beforeReceipts int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&beforeReceipts); err != nil {
		t.Fatal(err)
	}
	views := []struct {
		name, path string
		value      any
		fields     []string
		read       func(*client.Client) error
	}{
		{"queue", "/api/v1/projects/project-one/reviews/queue", queue, []string{"items.0.head_sha", "items.0.base_sha", "items.0.recent_runs.0.head_sha", "items.0.latest_attempts.0.head_sha"}, func(c *client.Client) error { _, err := c.ReviewQueue(t.Context(), client.PageOptions{}); return err }},
		{"pull", "/api/v1/projects/project-one/reviews/pulls/1", pull, []string{"pull.head_sha", "pull.base_sha", "pull.recent_runs.0.head_sha", "pull.latest_attempts.0.head_sha", "attempts.0.head_sha"}, func(c *client.Client) error { _, err := c.ReviewPull(t.Context(), 1, ""); return err }},
		{"runs", "/api/v1/projects/project-one/reviews/pulls/1/runs", runs, []string{"items.0.head_sha"}, func(c *client.Client) error {
			_, err := c.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
			return err
		}},
		{"run", "/api/v1/projects/project-one/reviews/runs/" + detail.Run.ID, detail, []string{"run.head_sha", "findings.0.fix_sha"}, func(c *client.Client) error { _, err := c.ReviewRun(t.Context(), detail.Run.ID, ""); return err }},
	}
	for _, view := range views {
		original, err := json.Marshal(view.value)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range view.fields {
			for _, sha := range []string{strings.Repeat("A", 40), strings.Repeat("a", 39) + "B"} {
				t.Run(view.name+"/"+field+"/"+sha[:1]+sha[39:], func(t *testing.T) {
					var body map[string]any
					if err := json.Unmarshal(original, &body); err != nil {
						t.Fatal(err)
					}
					var node any = body
					parts := strings.Split(field, ".")
					for _, part := range parts[:len(parts)-1] {
						if object, ok := node.(map[string]any); ok {
							node = object[part]
						} else {
							index, err := strconv.Atoi(part)
							if err != nil {
								t.Fatal(err)
							}
							node = node.([]any)[index]
						}
					}
					node.(map[string]any)[parts[len(parts)-1]] = sha
					encoded, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					mock := newReviewMock(t, string(encoded), view.path)
					checkError(t, view.read(mock.client), "invalid_response")
				})
			}
		}
		// Empty source SHAs are deliberately retained as unknown. Clear only the
		// resolution timestamp that independently requires a known fix SHA.
		var empty map[string]any
		if err := json.Unmarshal(original, &empty); err != nil {
			t.Fatal(err)
		}
		var clear func(any)
		clear = func(node any) {
			switch value := node.(type) {
			case map[string]any:
				for k, v := range value {
					if k == "head_sha" || k == "base_sha" || k == "fix_sha" || k == "resolved_at" {
						value[k] = ""
					} else if k == "fixed" {
						value[k] = 0
					} else {
						clear(v)
					}
				}
			case []any:
				for _, v := range value {
					clear(v)
				}
			}
		}
		clear(empty)
		encoded, err := json.Marshal(empty)
		if err != nil {
			t.Fatal(err)
		}
		mock := newReviewMock(t, string(encoded), view.path)
		if err := view.read(mock.client); err != nil {
			t.Fatalf("%s unknown SHA rejected: %v", view.name, err)
		}
	}
	var afterEvents, afterReceipts int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&afterReceipts); err != nil {
		t.Fatal(err)
	}
	if beforeEvents != afterEvents || beforeReceipts != afterReceipts {
		t.Fatal("typed reads mutated durable records")
	}
	current, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil || current.Items[0].HeadSHA != queue.Items[0].HeadSHA {
		t.Fatal("invalid response handling changed retained source", err)
	}
}
