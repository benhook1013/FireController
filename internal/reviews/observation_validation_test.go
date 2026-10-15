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
