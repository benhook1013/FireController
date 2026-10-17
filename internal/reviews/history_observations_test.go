package reviews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func historyObservationFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	return f
}
func historyObservationCounts(t *testing.T, f *apiFixture, want map[string]int) {
	t.Helper()
	for table, expected := range want {
		var count int
		if err := f.store.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != expected {
			t.Fatal("unexpected typed retained count", table, count, expected, err)
		}
	}
}
func historyObservationPost(t *testing.T, f *apiFixture, req ObservationRequest, key string) Receipt {
	t.Helper()
	response := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, key, req)
	var receipt Receipt
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
		t.Fatal("versioned observation refused", response.Code, response.Body.String())
	}
	return receipt
}

func TestHistoryObservationVersionedReplayCASAndOldCursors(t *testing.T) {
	f := historyObservationFixture(t)
	base := "/api/v1/projects/" + f.project + "/reviews"
	pull := partialHistoryPull()
	pull.Runs = append(pull.Runs, RunInput{SourceRunID: "other-run", Channel: "cli", Scope: "focused", Outcome: "unknown", Findings: []Finding{}, CoverageLimits: []string{}})
	attempt := Attempt{SourceAttemptID: "attempt-one", Channel: "manual", State: "active", StartedAt: "2026-10-01T00:00:00Z", SourceRunID: pull.Runs[0].SourceRunID}
	pull.Attempts = []Attempt{attempt}
	initial := reviewRequest(0, []PullInput{pull, samplePull(9, "Merged retained")})
	firstResponse := f.call("POST", base+"/observations", f.admin, "versioned-first", initial)
	var first Receipt
	if firstResponse.Code != 201 || json.Unmarshal(firstResponse.Body.Bytes(), &first) != nil {
		t.Fatal(firstResponse.Code, firstResponse.Body.String())
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue?limit=1", &queue)
	var runPage RunsResponse
	nativeRead(t, f, "/pulls/8/runs?limit=1", &runPage)
	if !queue.HasMore || queue.Cursor == "" || !runPage.HasMore || runPage.Cursor == "" {
		t.Fatal("bounded initial continuations missing")
	}
	runID := runOpaqueID(f.project, initial.Source, pull.Runs[0].SourceRunID)
	var oldDetail RunDetailResponse
	oldBytes := nativeRead(t, f, "/runs/"+runID+"?snapshot="+first.ID, &oldDetail)
	duration := int64(60)
	pull.Attempts = append([]Attempt(nil), pull.Attempts...) // Keep the first request immutable for exact retry.
	pull.Attempts[0].State = "completed"
	pull.Attempts[0].FinishedAt = "2026-10-01T00:01:00Z"
	pull.Attempts[0].DurationSeconds = &duration
	second := historyObservationPost(t, f, reviewRequest(1, []PullInput{pull, samplePull(9, "Merged retained")}), "versioned-attempt-update")
	historyObservationCounts(t, f, map[string]int{"review_snapshots": 2, "review_run_versions": 2, "review_finding_versions": 1, "review_attempt_versions": 2, "review_pull_versions": 3, "events": 2, "receipts": 2})
	beforeReplay := historyViewsDigest(t, f)
	replay := f.call("POST", base+"/observations", f.admin, "versioned-first", initial)
	if replay.Code != 201 || replay.Body.String() != firstResponse.Body.String() || beforeReplay != historyViewsDigest(t, f) {
		t.Fatal("old exact-key replay changed receipt or state", replay.Code, replay.Body.String())
	}
	changed := initial
	changed.Source = "changed-source"
	for _, test := range []struct {
		name, key, token string
		req              ObservationRequest
		code             int
	}{
		{"changed same key", "versioned-first", f.admin, changed, 409},
		{"stale CAS", "new-stale-key", f.admin, initial, 409},
		{"read-only writer", "readonly-key", f.readToken, reviewRequest(2, []PullInput{pull}), 403},
	} {
		response := f.call("POST", base+"/observations", test.token, test.key, test.req)
		if response.Code != test.code || historyViewsDigest(t, f) != beforeReplay {
			t.Fatal("rejected observation changed state", test.name, response.Code)
		}
	}
	var tail QueueResponse
	nativeRead(t, f, "/queue?limit=1&cursor="+url.QueryEscape(queue.Cursor), &tail)
	var runTail RunsResponse
	nativeRead(t, f, "/pulls/8/runs?limit=1&cursor="+url.QueryEscape(runPage.Cursor), &runTail)
	if tail.SnapshotID != first.ID || len(tail.Items) != 1 || tail.Items[0].Number != 9 || tail.Items[0].State != "merged" || runTail.SnapshotID != first.ID || len(runTail.Items) != 1 || runTail.Items[0].ID != runID {
		t.Fatal("old continuation followed current pointer")
	}
	var current AttemptsResponse
	nativeRead(t, f, "/pulls/8/attempts", &current)
	var historical AttemptsResponse
	nativeRead(t, f, "/pulls/8/attempts?snapshot="+first.ID, &historical)
	if current.SnapshotID != second.ID || current.Items[0].State != "completed" || historical.Items[0].State != "active" || historical.Items[0].DurationSeconds != nil {
		t.Fatal("attempt version was not snapshot-qualified")
	}
	if historyViewsDigest(t, f) != beforeReplay {
		t.Fatal("GET changed typed state")
	}
	omitted := samplePull(8, pull.Title)
	thirdReq := reviewRequest(2, []PullInput{omitted})
	thirdReq.Retired = []RetiredPull{{Number: 9, Reason: "superseded"}}
	third := historyObservationPost(t, f, thirdReq, "versioned-omit")
	var empty RunsResponse
	nativeRead(t, f, "/pulls/8/runs", &empty)
	if len(empty.Items) != 0 {
		t.Fatal("omitted run remains current")
	}
	if r := f.call("GET", base+"/pulls/9", f.readToken, "", nil); r.Code != 404 {
		t.Fatal("retired pull remains current", r.Code)
	}
	var retired string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT reason FROM review_retired_pulls WHERE project_id=? AND snapshot_id=? AND number=9`, f.project, third.ID).Scan(&retired); err != nil || retired != "superseded" {
		t.Fatal("retirement not versioned", retired, err)
	}
	fourth := historyObservationPost(t, f, reviewRequest(3, []PullInput{samplePull(9, "Merged retained"), pull}), "versioned-return-reorder")
	historyObservationCounts(t, f, map[string]int{"review_run_versions": 2, "review_finding_versions": 1, "review_attempt_versions": 2, "review_retired_versions": 1})
	var afterReturn RunsResponse
	nativeRead(t, f, "/pulls/8/runs", &afterReturn)
	if afterReturn.SnapshotID != fourth.ID || len(afterReturn.Items) != 2 {
		t.Fatal("return failed")
	}
	var oldGap RunsResponse
	nativeRead(t, f, "/pulls/8/runs?snapshot="+third.ID, &oldGap)
	if len(oldGap.Items) != 0 {
		t.Fatal("return resurrected omitted gap")
	}
	var pinned RunDetailResponse
	if got := nativeRead(t, f, "/runs/"+runID+"?snapshot="+first.ID, &pinned); string(got) != string(oldBytes) {
		t.Fatal("later observations changed old exact detail")
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryObservationFinalizedCumulativeFindingsSurviveOmission(t *testing.T) {
	f := historyObservationFixture(t)
	pull := partialHistoryPull()
	first := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "finalized-first")
	var original RunDetailResponse
	originalBytes := nativeRead(t, f, "/runs/"+runOpaqueID(f.project, "test-import", pull.Runs[0].SourceRunID)+"?snapshot="+first.ID, &original)
	empty := samplePull(pull.Number, pull.Title)
	historyObservationPost(t, f, reviewRequest(1, []PullInput{empty}), "finalized-gap")
	pull.Runs[0].Findings = append(pull.Runs[0].Findings, Finding{SourceFindingID: "second", Title: "Second known title", Detail: "Second known body", Severity: "low", Disposition: "rejected"})
	enriched := historyObservationPost(t, f, reviewRequest(2, []PullInput{pull}), "finalized-enriched")
	historyObservationPost(t, f, reviewRequest(3, []PullInput{empty}), "finalized-next-gap")
	for _, test := range []struct {
		name   string
		change func(*PullInput)
	}{
		{"drop-known", func(p *PullInput) { p.Runs[0].Findings = p.Runs[0].Findings[:1] }},
		{"replace-first", func(p *PullInput) { p.Runs[0].Findings[0].Detail = "replacement" }},
		{"replace-added", func(p *PullInput) { p.Runs[0].Findings[1].Title = "replacement" }},
		{"change-count", func(p *PullInput) { p.Runs[0].Found++ }},
		{"change-owner", func(p *PullInput) { p.Number = 9 }},
		{"change-comparison", func(p *PullInput) { p.Runs[0].Scope = "changed" }},
		{"reopen", func(p *PullInput) { p.Runs[0].Finalized = false }},
	} {
		var changed PullInput
		encoded, _ := json.Marshal(pull)
		if err := json.Unmarshal(encoded, &changed); err != nil {
			t.Fatal(err)
		}
		test.change(&changed)
		req := reviewRequest(4, []PullInput{changed})
		if changed.Number != pull.Number {
			req.Queue = append([]PullInput{empty}, req.Queue...)
		}
		before := historyViewsDigest(t, f)
		response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, test.name, req)
		if response.Code != 409 || !strings.Contains(response.Body.String(), "observation_conflict") || before != historyViewsDigest(t, f) {
			t.Fatal("finalized history protection lost", test.name, response.Code, response.Body.String())
		}
	}
	pull.Runs[0].Findings[0].ProofNote = "Reviewed correction"
	historyObservationPost(t, f, reviewRequest(4, []PullInput{pull}), "finalized-correction")
	historyObservationCounts(t, f, map[string]int{"review_run_versions": 3, "review_finding_versions": 5})
	var old RunDetailResponse
	if got := nativeRead(t, f, "/runs/"+runOpaqueID(f.project, "test-import", pull.Runs[0].SourceRunID)+"?snapshot="+first.ID, &old); string(got) != string(originalBytes) {
		t.Fatal("correction rewrote original")
	}
	var added RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, "test-import", pull.Runs[0].SourceRunID)+"?snapshot="+enriched.ID, &added)
	if len(added.Findings) != 2 || added.Findings[0].ProofNote != "" {
		t.Fatal("later correction rewrote enrichment")
	}
	rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+finalizedRunQuery, f.project, "test-import", pull.Runs[0].SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "review_run_versions_source_revision") || strings.Contains(joined, "TEMP B-TREE") || strings.Contains(joined, "SCAN review_run_versions") {
		t.Fatal("finalized lookup did not use exact producer/version index", joined)
	}
}

func TestHistoryObservationRunAndAttemptIdentityAcrossOmission(t *testing.T) {
	f := historyObservationFixture(t)
	pull := samplePull(8, "Identity owner")
	pull.Runs = []RunInput{{SourceRunID: "nonfinalized", Channel: "cli", HeadSHA: strings.Repeat("a", 40), Scope: "focused", Outcome: "unknown"}}
	pull.Attempts = []Attempt{{SourceAttemptID: "settled", Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: "completed", StartedAt: "2026-10-01T00:00:00Z", FinishedAt: "2026-10-01T00:01:00Z", TriggerID: "known-trigger"}}
	historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "identity-first")
	empty := samplePull(8, pull.Title)
	historyObservationPost(t, f, reviewRequest(1, []PullInput{empty}), "identity-gap")
	for index, change := range []func(*PullInput){func(p *PullInput) { p.Number = 9 }, func(p *PullInput) { p.Runs[0].HeadSHA = strings.Repeat("b", 40) }, func(p *PullInput) { p.Attempts[0].State = "active" }, func(p *PullInput) { p.Attempts[0].TriggerID = "" }} {
		var candidate PullInput
		raw, _ := json.Marshal(pull)
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatal(err)
		}
		change(&candidate)
		req := reviewRequest(2, []PullInput{candidate})
		if candidate.Number != 8 {
			req.Queue = append([]PullInput{empty}, req.Queue...)
		}
		before := historyViewsDigest(t, f)
		response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, fmt.Sprintf("identity-conflict-%d", index), req)
		if response.Code != 409 || before != historyViewsDigest(t, f) {
			t.Fatal("omission reset source identity", index, response.Code, response.Body.String())
		}
	}
	historyObservationPost(t, f, reviewRequest(2, []PullInput{pull}), "identity-return")
	historyObservationCounts(t, f, map[string]int{"review_run_versions": 1, "review_attempt_versions": 1})
}
