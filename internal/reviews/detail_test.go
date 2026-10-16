package reviews

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func largeDetailRun() RunInput {
	run := nativeUndecidedRun("large-retained-source", 100)
	for i := range run.Findings {
		run.Findings[i].Title = strings.Repeat("<", 500)
		run.Findings[i].Detail = strings.Repeat("x", 16<<10)
	}
	return run
}

func largeDetailDecisions(run RunInput, proofLength int) NativeAdjudication {
	input := nativeAdjudicationInput(run)
	for i := range input.Findings {
		finding := &input.Findings[i]
		decideNativeFinding(finding, "accepted")
		finding.DecisionReason = strings.Repeat("<", 2048)
		finding.FixSHA = strings.Repeat("f", 40)
		finding.ProofNote = strings.Repeat("<", proofLength)
		finding.ResolvedAt = "2026-01-01T00:05:00Z"
	}
	return input
}

func TestNativeDetailBudgetRejectsUpdateAtomicallyAndAllowsNearBudgetRead(t *testing.T) {
	f := newAPIFixture(t)
	run := largeDetailRun()
	// The immutable source is small enough for trusted CLI evidence ingestion;
	// adding individually valid, escaped decision/proof text causes the overflow.
	review := reviewprovider.CLIReview{Outcome: "completed", Complete: true, ReviewedFiles: []string{"source.go"}, ReportedFindings: 100}
	for _, finding := range run.Findings {
		review.Findings = append(review.Findings, reviewprovider.CLIFinding{Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity, Path: "source.go", StartLine: 1, EndLine: 1})
	}
	source, err := json.Marshal(review)
	if err != nil || len(source)+8192 >= 2<<20 {
		t.Fatalf("source plus envelope allowance must fit capture input: %d, %v", len(source), err)
	}
	prior := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	runID := runOpaqueID(f.project, nativeSource, run.SourceRunID)
	var before RunDetailResponse
	beforeBody := nativeRead(t, f, "/runs/"+runID, &before)
	counts := nativeProjectionCounts(t, f)
	oldSnapshot := nativeSnapshotBytes(t, f, prior.ID)
	input := largeDetailDecisions(run, 2048)
	// Demonstrate that the rejected payload really exceeds the encoder budget.
	projected := before
	projected.Findings = append([]Finding(nil), before.Findings...)
	for i, next := range input.Findings {
		finding := &projected.Findings[i]
		finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt = next.Disposition, next.DecisionReason, next.Actor, next.DecidedAt
		finding.FixSHA, finding.ProofNote, finding.ResolvedAt = next.FixSHA, next.ProofNote, next.ResolvedAt
	}
	encoded, err := json.Marshal(projected)
	if err != nil || len(encoded)+1 <= maxRunDetailBytes {
		t.Fatal("regression payload did not exceed detail budget", len(encoded), err)
	}
	_, _, err = applyNativeAdjudication(f, input)
	nativeErrorCode(t, err, "workspace_capacity")
	if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) || oldSnapshot != nativeSnapshotBytes(t, f, prior.ID) {
		t.Fatal("rejected update changed retained rows")
	}
	var after RunDetailResponse
	if afterBody := nativeRead(t, f, "/runs/"+runID, &after); !bytes.Equal(beforeBody, afterBody) {
		t.Fatal("rejected update changed current pointer or source")
	}

	// A nearby valid result still fits without truncation or a larger read cap.
	near := largeDetailDecisions(run, 1630)
	receipt, changed, err := applyNativeAdjudication(f, near)
	if err != nil || !changed || receipt.Revision != prior.Revision+1 {
		t.Fatal("near-budget update failed", receipt, changed, err)
	}
	body := nativeRead(t, f, "/runs/"+runID, &after)
	if len(body) > maxRunDetailBytes || len(body) < maxRunDetailBytes-(maxRunDetailBytes/100) || body[len(body)-1] != '\n' {
		t.Fatalf("expected full response within 1%% of budget, including newline: %d", len(body))
	}
	if after.Run.Fixed != 100 || after.Run.Accepted != 100 || len(after.Findings) != 100 {
		t.Fatal("near-budget detail was truncated")
	}
	for i, finding := range after.Findings {
		if finding.Title != run.Findings[i].Title || finding.Detail != run.Findings[i].Detail || finding.ProofNote != near.Findings[i].ProofNote || finding.DecisionReason != near.Findings[i].DecisionReason {
			t.Fatal("near-budget bytes were changed")
		}
	}
	if oldSnapshot != nativeSnapshotBytes(t, f, prior.ID) {
		t.Fatal("successful update rewrote old snapshot")
	}
}

func TestNativeDetailBudgetRejectsInitialPublicationAtomically(t *testing.T) {
	f := newAPIFixture(t)
	f.observe(t, reviewRequest(0, []PullInput{nativePull(2)}), "retained-import")
	var queue QueueResponse
	beforeBody := nativeRead(t, f, "/queue", &queue)
	oldSnapshot := nativeSnapshotBytes(t, f, queue.SnapshotID)
	counts := nativeProjectionCounts(t, f)
	run := largeDetailRun()
	input := largeDetailDecisions(run, 2048)
	run.Accepted, run.Finalized = 100, true
	for i, next := range input.Findings {
		finding := &run.Findings[i]
		finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt = next.Disposition, next.DecisionReason, next.Actor, next.DecidedAt
		finding.FixSHA, finding.ProofNote, finding.ResolvedAt = next.FixSHA, next.ProofNote, next.ResolvedAt
	}
	pull := nativePull(1)
	pull.Runs = []RunInput{run}
	request := reviewRequest(queue.Revision, []PullInput{pull})
	request.Source = nativeSource
	if err := validateObservation(&request); err != nil {
		t.Fatal("oversized publication must satisfy individual field limits", err)
	}
	_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Run: &run})
	nativeErrorCode(t, err, "workspace_capacity")
	if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) || oldSnapshot != nativeSnapshotBytes(t, f, queue.SnapshotID) {
		t.Fatal("rejected publication changed retained rows")
	}
	if afterBody := nativeRead(t, f, "/queue", &queue); !bytes.Equal(beforeBody, afterBody) {
		t.Fatal("rejected publication moved current snapshot")
	}
	w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), f.readToken, "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatal("rejected run became visible", w.Code)
	}
}

func TestRunDetailBudgetMatchesHTTPEncoding(t *testing.T) {
	response := RunDetailResponse{Findings: []Finding{{Title: "<>&\u2028"}}}
	size, err := encodedRunDetailSize(response)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	core.WriteJSON(w, http.StatusOK, response)
	if size != w.Body.Len() || w.Body.Bytes()[w.Body.Len()-1] != '\n' {
		t.Fatalf("budget must include exact HTTP escaping and final newline: measured %d, sent %d", size, w.Body.Len())
	}
}
