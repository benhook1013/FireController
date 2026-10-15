package reviews

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
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type apiFixture struct {
	store                     *core.Store
	mux                       http.Handler
	admin, readToken, project string
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &apiFixture{store: store, admin: strings.TrimSpace(string(data)), readToken: "reviews-read-test-token-000000000000"}
	if _, err := store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('project-review-test','Review test',701,'owner/reviews')`); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(f.readToken))
	if _, err := store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES('review-read-credential','review-read-principal',?,0,0)`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('review-read-credential','project-review-test',1,0)`); err != nil {
		t.Fatal(err)
	}
	f.project = "project-review-test"
	mux := http.NewServeMux()
	Register(mux, store)
	f.mux = mux
	return f
}

func (f *apiFixture) call(method, path, token, key string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *apiFixture) observe(t *testing.T, req ObservationRequest, key string) map[string]any {
	t.Helper()
	w := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, key, req)
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("observation: %d %s", w.Code, w.Body.String())
	}
	return value
}

func reviewRequest(revision int64, queue []PullInput) ObservationRequest {
	return ObservationRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "test-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: queue}
}
func samplePull(number int64, title string) PullInput {
	return PullInput{Number: number, Title: title, State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable", Complete: true}
}

func TestQueueImportReplayAndSnapshotStableCursor(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	initial := f.call("GET", base+"/queue", f.readToken, "", nil)
	if initial.Code != 200 {
		t.Fatalf("empty queue: %d %s", initial.Code, initial.Body.String())
	}
	var empty QueueResponse
	if err := json.Unmarshal(initial.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Configured || empty.Revision != 0 || len(empty.Items) != 0 || empty.Items == nil {
		t.Fatalf("bad unconfigured shape: %+v", empty)
	}
	req := reviewRequest(0, []PullInput{samplePull(4, "PR 4"), samplePull(1, "Merged history")})
	firstResponse := f.call("POST", base+"/observations", f.admin, "observe-1", req)
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first observation: %d %s", firstResponse.Code, firstResponse.Body.String())
	}
	replay := f.call("POST", base+"/observations", f.admin, "observe-1", req)
	if replay.Code != 201 || replay.Body.String() != firstResponse.Body.String() {
		t.Fatalf("exact replay changed: %d %s", replay.Code, replay.Body.String())
	}
	page := f.call("GET", base+"/queue?limit=1", f.readToken, "", nil)
	var firstPage QueueResponse
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &firstPage) != nil {
		t.Fatalf("first queue page: %d %s", page.Code, page.Body.String())
	}
	if len(firstPage.Items) != 1 || firstPage.Items[0].Number != 4 || !firstPage.HasMore || firstPage.Cursor == "" {
		t.Fatalf("unexpected compact page: %+v", firstPage)
	}
	encoded, _ := json.Marshal(firstPage)
	if strings.Contains(string(encoded), "findings") || strings.Contains(string(encoded), "detail") || firstPage.SnapshotID == "" {
		t.Fatalf("queue exposed detail or missing snapshot: %s", encoded)
	}
	newer := reviewRequest(1, []PullInput{samplePull(4, "Updated PR 4"), samplePull(1, "Merged history")})
	f.observe(t, newer, "observe-2")
	continued := f.call("GET", base+"/queue?limit=1&cursor="+firstPage.Cursor, f.readToken, "", nil)
	var second QueueResponse
	if continued.Code != 200 || json.Unmarshal(continued.Body.Bytes(), &second) != nil {
		t.Fatalf("stable cursor: %d %s", continued.Code, continued.Body.String())
	}
	if second.Revision != 1 || second.SnapshotID != firstPage.SnapshotID || len(second.Items) != 1 || second.Items[0].Number != 1 {
		t.Fatalf("cursor did not retain original snapshot: %+v", second)
	}
	wrong := f.call("GET", base+"/queue?limit=1&cursor="+firstPage.Cursor, "", "", nil)
	if wrong.Code != 401 {
		t.Fatalf("unauthenticated cursor read: %d", wrong.Code)
	}
}

func TestReviewDetailsKeepCompactAndFullBoundaries(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	run := RunInput{SourceRunID: "run-1", Channel: "manual", Scope: "focused", Outcome: "completed", Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Accepted: 1, Finalized: true, CoverageLimits: []string{"source-only"}, Findings: []Finding{{SourceFindingID: "finding-1", Title: "Safe title", Detail: "private finding body", Severity: "high", Disposition: "accepted", FixSHA: strings.Repeat("a", 40)}}}
	p := samplePull(2, "PR 2")
	p.Runs = []RunInput{run}
	duration := int64(60)
	p.Attempts = []Attempt{{SourceAttemptID: "attempt-sub", Channel: "subagent", State: "completed", StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, DurationSeconds: &duration, SourceRunID: "run-1"}, {SourceAttemptID: "attempt-cli", Channel: "cli", State: "failed"}}
	created := f.observe(t, reviewRequest(0, []PullInput{p}), "detail-observe")
	snapshot := created["id"].(string)
	queue := f.call("GET", base+"/queue", f.readToken, "", nil)
	if strings.Contains(queue.Body.String(), "private finding body") {
		t.Fatal("queue leaked finding detail")
	}
	pull := f.call("GET", base+"/pulls/2?snapshot="+snapshot, f.readToken, "", nil)
	if pull.Code != 200 || strings.Contains(pull.Body.String(), "private finding body") {
		t.Fatalf("pull summary leaked finding or failed: %d %s", pull.Code, pull.Body.String())
	}
	if !strings.Contains(pull.Body.String(), "subagent") {
		t.Fatalf("historical attempt channel missing: %s", pull.Body.String())
	}
	var pullResult PullResponse
	if err := json.Unmarshal(pull.Body.Bytes(), &pullResult); err != nil || len(pullResult.Attempts) != 2 || pullResult.Attempts[0].DurationSeconds == nil || *pullResult.Attempts[0].DurationSeconds != 60 || pullResult.Attempts[1].DurationSeconds != nil {
		t.Fatalf("known/unknown durations were not retained: %v %+v", err, pullResult.Attempts)
	}

	runs := f.call("GET", base+"/pulls/2/runs?snapshot="+snapshot, f.readToken, "", nil)
	if runs.Code != 200 || strings.Contains(runs.Body.String(), "private finding body") || !strings.Contains(runs.Body.String(), "source-only") {
		t.Fatalf("run summaries incorrect: %d %s", runs.Code, runs.Body.String())
	}
	var runPage RunsResponse
	if err := json.Unmarshal(runs.Body.Bytes(), &runPage); err != nil || len(runPage.Items) != 1 {
		t.Fatalf("run list decode: %v %+v", err, runPage)
	}
	detail := f.call("GET", base+"/runs/"+runPage.Items[0].ID+"?snapshot="+snapshot, f.readToken, "", nil)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "private finding body") {
		t.Fatalf("explicit run detail missing finding: %d %s", detail.Code, detail.Body.String())
	}
	noAccess := f.call("GET", base+"/runs/"+runPage.Items[0].ID+"?snapshot="+snapshot, f.readToken, "", nil)
	if noAccess.Code != 200 {
		t.Fatalf("repeat detail: %d", noAccess.Code)
	}
}

func TestReviewObservationCASRetirementAndAuthorization(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	for _, number := range []string{"01", "+1"} {
		if response := f.call("GET", base+"/pulls/"+number, f.readToken, "", nil); response.Code != http.StatusBadRequest {
			t.Errorf("noncanonical pull number %q: %d %s", number, response.Code, response.Body.String())
		}
	}
	if got := f.call("POST", base+"/observations", f.readToken, "read-denied", reviewRequest(0, []PullInput{})); got.Code != 403 {
		t.Fatalf("read-only import: %d %s", got.Code, got.Body.String())
	}
	first := reviewRequest(0, []PullInput{samplePull(1, "Merged")})
	f.observe(t, first, "first")
	stale := reviewRequest(0, []PullInput{samplePull(1, "Changed")})
	if got := f.call("POST", base+"/observations", f.admin, "stale", stale); got.Code != 409 {
		t.Fatalf("stale CAS: %d %s", got.Code, got.Body.String())
	}
	badOmit := reviewRequest(1, []PullInput{})
	if got := f.call("POST", base+"/observations", f.admin, "omit", badOmit); got.Code != 400 {
		t.Fatalf("silent retirement: %d %s", got.Code, got.Body.String())
	}
	retire := reviewRequest(1, []PullInput{})
	retire.Retired = []RetiredPull{{Number: 1, Reason: "superseded"}}
	f.observe(t, retire, "retire")
	if got := f.call("GET", "/api/v1/projects/other/reviews/queue", f.readToken, "", nil); got.Code == 200 {
		t.Fatal("cross-project read succeeded")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='review-read-credential'`); err != nil {
		t.Fatal(err)
	}
	if got := f.call("GET", base+"/queue", f.readToken, "", nil); got.Code != 401 {
		t.Fatalf("revoked read: %d", got.Code)
	}
}

func TestReviewImportRepositoryFinalizedCountsAndAtomicRetry(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	badRepository := reviewRequest(0, []PullInput{samplePull(8, "PR 8")})
	badRepository.RepositoryID = 702
	if got := f.call("POST", base+"/observations", f.admin, "repo-mismatch", badRepository); got.Code != http.StatusBadRequest {
		t.Fatalf("repository mismatch: %d %s", got.Code, got.Body.String())
	}
	if got := f.call("GET", base+"/queue", f.admin, "", nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"configured":false`) {
		t.Fatalf("repository mismatch committed a snapshot: %d %s", got.Code, got.Body.String())
	}

	run := RunInput{SourceRunID: "fixed-source-run", Channel: "manual", Scope: "focused", Outcome: "completed", Complete: true,
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Accepted: 1, Finalized: true,
		Findings: []Finding{{SourceFindingID: "finding", Title: "Finding", Severity: "medium", Disposition: "accepted"}}}
	pull := samplePull(8, "PR 8")
	pull.Runs = []RunInput{run}
	first := reviewRequest(0, []PullInput{pull})
	f.observe(t, first, "finalized-first")
	changedRun := run
	changedRun.Found, changedRun.Accepted = 2, 2
	changedRun.Findings = append(run.Findings, Finding{SourceFindingID: "finding-2", Title: "Another", Severity: "low", Disposition: "accepted"})
	changedPull := pull
	changedPull.Runs = []RunInput{changedRun}
	changed := reviewRequest(1, []PullInput{changedPull})
	if got := f.call("POST", base+"/observations", f.admin, "finalized-change", changed); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "observation_conflict") {
		t.Fatalf("finalized counts changed: %d %s", got.Code, got.Body.String())
	}
	if countRows(t, f.store, "review_snapshots") != 1 || countRows(t, f.store, "events") != 1 || countRows(t, f.store, "receipts") != 1 {
		t.Fatal("finalized-count conflict left durable side effects")
	}

	rollback := reviewRequest(1, []PullInput{samplePull(8, "PR 8 retry")})
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_review_pull BEFORE INSERT ON review_pulls BEGIN SELECT RAISE(ABORT,'private SQL detail'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.call("POST", base+"/observations", f.admin, "atomic-retry", rollback)
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), "private SQL detail") {
		t.Fatalf("unsafe/incorrect insertion failure: %d %s", failed.Code, failed.Body.String())
	}
	if countRows(t, f.store, "review_snapshots") != 1 || countRows(t, f.store, "review_workspace") != 1 || countRows(t, f.store, "events") != 1 || countRows(t, f.store, "receipts") != 1 {
		t.Fatal("failed observation was partially committed")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_review_pull`); err != nil {
		t.Fatal(err)
	}
	retried := f.call("POST", base+"/observations", f.admin, "atomic-retry", rollback)
	if retried.Code != http.StatusCreated || countRows(t, f.store, "review_snapshots") != 2 || countRows(t, f.store, "events") != 2 || countRows(t, f.store, "receipts") != 2 {
		t.Fatalf("same-key retry did not commit once: %d %s", retried.Code, retried.Body.String())
	}
}

func TestFinalizedRunIdentityAndFindingTextAreImmutableButProofCanImprove(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	run := RunInput{SourceRunID: "stable-run", Channel: "manual", HeadSHA: strings.Repeat("a", 40), Reviewer: "reviewer", Scope: "focused", Outcome: "completed", Complete: true,
		StartedAt: "2026-02-01T00:00:00Z", FinishedAt: "2026-02-01T00:01:00Z", Found: 1, Accepted: 1, Finalized: true,
		Findings: []Finding{{SourceFindingID: "stable-finding", Title: "Original title", Detail: "Original source detail", Severity: "medium", Disposition: "accepted"}}}
	initialPull := samplePull(8, "PR 8")
	initialPull.Runs = []RunInput{run}
	f.observe(t, reviewRequest(0, []PullInput{initialPull}), "immutable-first")

	identityMutations := []struct {
		name   string
		mutate func(*PullInput)
	}{
		{"different pull", func(p *PullInput) { p.Number = 9 }},
		{"different channel", func(p *PullInput) { p.Runs[0].Channel = "subagent" }},
		{"different head", func(p *PullInput) { p.Runs[0].HeadSHA = strings.Repeat("b", 40) }},
		{"different reviewer", func(p *PullInput) { p.Runs[0].Reviewer = "other-reviewer" }},
		{"different scope", func(p *PullInput) { p.Runs[0].Scope = "broader" }},
		{"different finding id", func(p *PullInput) { p.Runs[0].Findings[0].SourceFindingID = "replacement-id" }},
		{"different finding title", func(p *PullInput) { p.Runs[0].Findings[0].Title = "Replacement title" }},
		{"different finding detail", func(p *PullInput) { p.Runs[0].Findings[0].Detail = "Replacement source detail" }},
	}
	for i, test := range identityMutations {
		pull := initialPull
		pull.Runs = append([]RunInput(nil), initialPull.Runs...)
		pull.Runs[0].Findings = append([]Finding(nil), run.Findings...)
		test.mutate(&pull)
		request := reviewRequest(1, []PullInput{pull})
		if pull.Number != initialPull.Number {
			request.Retired = []RetiredPull{{Number: initialPull.Number, Reason: "superseded"}}
		}
		response := f.call("POST", base+"/observations", f.admin, "immutable-conflict-"+strconv.Itoa(i), request)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "observation_conflict") {
			t.Errorf("%s was not rejected: %d %s", test.name, response.Code, response.Body.String())
		}
	}

	correctionPull := initialPull
	correctionPull.Runs = append([]RunInput(nil), initialPull.Runs...)
	correctionPull.Runs[0].Findings = append([]Finding(nil), run.Findings...)
	correctionPull.Runs[0].Findings[0].FixSHA = strings.Repeat("c", 40)
	correctionPull.Runs[0].Findings[0].ProofNote = "Verified in the accepted correction"
	correctionPull.Runs[0].Findings[0].ResolvedAt = "2026-02-02T00:00:00Z"
	correction := reviewRequest(1, []PullInput{correctionPull})
	if response := f.call("POST", base+"/observations", f.admin, "proof-correction", correction); response.Code != http.StatusCreated {
		t.Fatalf("valid fix-proof correction rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestPartialFinalizedRunRetainsOnlySuppliedSourceFindings(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	run := RunInput{SourceRunID: "partial-finalized", Channel: "subagent", HeadSHA: strings.Repeat("d", 40), Scope: "bounded", Outcome: "unknown", Complete: false,
		Found: 2, Accepted: 0, Finalized: true, Findings: []Finding{{SourceFindingID: "observed-one", Title: "Observed title", Detail: "Observed detail", Severity: "unknown", Disposition: "unadjudicated"}}}
	pull := samplePull(3, "Partial capture")
	pull.Runs = []RunInput{run}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "partial-first")
	if response := f.call("POST", base+"/observations", f.admin, "partial-repeat", reviewRequest(1, []PullInput{pull})); response.Code != http.StatusCreated {
		t.Fatalf("partial finalized source incorrectly required fabricated missing finding: %d %s", response.Code, response.Body.String())
	}
}

func countRows(t *testing.T, store *core.Store, table string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
