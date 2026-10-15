package reviews

import (
	"net/http"
	"strings"
	"testing"
)

func TestObservationRejectsBlankCoverageAndUnacceptedProofWithoutEffects(t *testing.T) {
	for _, test := range []struct {
		name               string
		limits             []string
		disposition, proof string
	}{
		{"empty-coverage", []string{""}, "accepted", ""},
		{"blank-coverage", []string{" \t\r\n"}, "accepted", ""},
		{"unadjudicated-proof", nil, "unadjudicated", "Proof belongs to an accepted finding"},
		{"routed-proof", nil, "routed", "Proof belongs to an accepted finding"},
		{"rejected-proof", nil, "rejected", "Proof belongs to an accepted finding"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			pull := samplePull(2, "Scoped review")
			pull.Runs = []RunInput{{SourceRunID: "source", Channel: "manual", Scope: "bounded", Outcome: "unknown", Found: 1, CoverageLimits: test.limits, Findings: []Finding{{SourceFindingID: "finding", Title: "Retained title", Severity: "medium", Disposition: test.disposition, ProofNote: test.proof}}}}
			before := []int{countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events"), countRows(t, f.store, "receipts")}
			response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "invalid-shape", reviewRequest(0, []PullInput{pull}))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "validation_failed") {
				t.Fatal("invalid view shape admitted", response.Code, response.Body.String())
			}
			if before[0] != countRows(t, f.store, "review_snapshots") || before[1] != countRows(t, f.store, "events") || before[2] != countRows(t, f.store, "receipts") {
				t.Fatal("invalid observation changed durable state")
			}
		})
	}
}

func TestObservationAcceptedProofAndNonblankCoverageRemainReadable(t *testing.T) {
	f := newAPIFixture(t)
	pull := samplePull(2, "Scoped review")
	pull.Runs = []RunInput{{SourceRunID: "source", Channel: "manual", Scope: "bounded", Outcome: "unknown", Found: 1, CoverageLimits: []string{"  Retained scope limitation\n"}, Findings: []Finding{{SourceFindingID: "finding", Title: "Title", Severity: "medium", Disposition: "accepted", ProofNote: "Accepted proof without a known fix SHA"}}}}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "valid-shape")
	response := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/runs/"+runOpaqueID(f.project, "test-import", "source"), f.readToken, "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Accepted proof without a known fix SHA") {
		t.Fatal("accepted proof was rejected or lost", response.Code, response.Body.String())
	}
}

func TestObservationRejectsUnorderedFailedRunWithoutEffects(t *testing.T) {
	f := newAPIFixture(t)
	pull := samplePull(2, "Retained review")
	f.observe(t, reviewRequest(0, []PullInput{pull}), "initial-timing")
	var originalSnapshot string
	if err := f.store.DB().QueryRow(`SELECT snapshot_id FROM review_workspace WHERE project_id=?`, f.project).Scan(&originalSnapshot); err != nil {
		t.Fatal(err)
	}
	before := map[string]int{}
	for _, table := range []string{"review_snapshots", "review_pulls", "review_runs", "review_findings", "events", "receipts"} {
		before[table] = countRows(t, f.store, table)
	}
	pull.Runs = []RunInput{{SourceRunID: "failed-source", Channel: "cli", Scope: "bounded", Outcome: "failed", StartedAt: "2026-01-02T03:04:06Z", FinishedAt: "2026-01-02T03:04:05Z"}}
	response := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "unordered-failed-run", reviewRequest(1, []PullInput{pull}))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "validation_failed") {
		t.Fatalf("unordered failed run admitted: %d %s", response.Code, response.Body.String())
	}
	for table, count := range before {
		if got := countRows(t, f.store, table); got != count {
			t.Fatalf("rejected observation changed %s: %d to %d", table, count, got)
		}
	}
	var snapshot string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT snapshot_id,revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&snapshot, &revision); err != nil {
		t.Fatal(err)
	}
	if snapshot != originalSnapshot || revision != 1 {
		t.Fatal("rejected observation advanced the current workspace")
	}
}

func TestRunTimestampOrderingAndIncompleteShapes(t *testing.T) {
	const start = "2026-01-02T03:04:05Z"
	const finish = "2026-01-02T03:04:06Z"
	for _, outcome := range []string{"completed", "active", "prepared", "posting", "failed", "cancelled", "rate_limited", "uncertain", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			if err := validateRunTimes(RunInput{Outcome: outcome, StartedAt: finish, FinishedAt: start}); err == nil {
				t.Fatal("unordered timestamps accepted")
			}
			for _, end := range []string{start, finish} {
				if err := validateRunTimes(RunInput{Outcome: outcome, StartedAt: start, FinishedAt: end}); err != nil {
					t.Fatalf("ordered timestamps rejected: %v", err)
				}
			}
			for _, times := range [][2]string{{"", ""}, {start, ""}, {"", finish}} {
				for _, complete := range []bool{false, true} {
					err := validateRunTimes(RunInput{Outcome: outcome, Complete: complete, StartedAt: times[0], FinishedAt: times[1]})
					if required := complete || outcome == "completed"; (err != nil) != required {
						t.Fatalf("complete=%v timestamps=%q error=%v", complete, times, err)
					}
				}
			}
		})
	}
}
