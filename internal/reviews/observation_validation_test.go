package reviews

import (
	"encoding/json"
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

func TestObservationRejectsNoncanonicalSHAWithoutPoisoningFinalizedHistory(t *testing.T) {
	f := newAPIFixture(t)
	pull := samplePull(2, "Canonical source history")
	pull.HeadSHA, pull.BaseSHA = strings.Repeat("a", 40), strings.Repeat("b", 40)
	pull.Runs = []RunInput{{SourceRunID: "canonical-source", Channel: "cli", HeadSHA: pull.HeadSHA, Scope: "full comparison", Outcome: "completed", Complete: true, Finalized: true, Found: 1, Accepted: 1, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Findings: []Finding{{SourceFindingID: "canonical-finding", Title: "Retained immutable finding", Detail: "Literal evidence", Severity: "medium", Disposition: "accepted", FixSHA: strings.Repeat("c", 40), ProofNote: "Focused proof"}}}}
	pull.Attempts = []Attempt{{SourceAttemptID: "canonical-attempt", Channel: "cli", HeadSHA: pull.HeadSHA, State: "completed", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", SourceRunID: "canonical-source"}}
	base := "/api/v1/projects/" + f.project + "/reviews"
	original := reviewRequest(0, []PullInput{pull})
	receipt := f.call(http.MethodPost, base+"/observations", f.admin, "canonical-history", original)
	if receipt.Code != http.StatusCreated {
		t.Fatal(receipt.Code, receipt.Body.String())
	}
	oldDetail := f.call(http.MethodGet, base+"/runs/"+runOpaqueID(f.project, "test-import", "canonical-source"), f.readToken, "", nil)
	if oldDetail.Code != http.StatusOK {
		t.Fatal(oldDetail.Body.String())
	}
	tables := []string{"review_workspace", "review_snapshots", "review_pulls", "review_runs", "review_attempts", "review_findings", "events", "receipts"}
	counts := map[string]int{}
	for _, table := range tables {
		counts[table] = countRows(t, f.store, table)
	}
	for _, field := range []string{"pull-head", "pull-base", "run-head", "attempt-head", "finding-fix"} {
		for _, value := range []string{strings.Repeat("A", 40), "aB" + strings.Repeat("c", 38)} {
			var changed PullInput
			raw, err := json.Marshal(pull)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "pull-head":
				changed.HeadSHA = value
			case "pull-base":
				changed.BaseSHA = value
			case "run-head":
				changed.Runs[0].HeadSHA = value
			case "attempt-head":
				changed.Attempts[0].HeadSHA = value
			case "finding-fix":
				changed.Runs[0].Findings[0].FixSHA = value
			}
			response := f.call(http.MethodPost, base+"/observations", f.admin, "case-rejected-key", reviewRequest(1, []PullInput{changed}))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "validation_failed") {
				t.Fatalf("%s %q admitted noncanonical SHA: %d %s", field, value, response.Code, response.Body.String())
			}
			for _, table := range tables {
				if countRows(t, f.store, table) != counts[table] {
					t.Fatalf("rejected %s changed %s", field, table)
				}
			}
			detail := f.call(http.MethodGet, base+"/runs/"+runOpaqueID(f.project, "test-import", "canonical-source"), f.readToken, "", nil)
			if detail.Code != http.StatusOK || detail.Body.String() != oldDetail.Body.String() {
				t.Fatal("invalid import altered finalized source/detail")
			}
		}
	}
	// A validation failure consumes no key; canonical input can still append.
	advanced := f.call(http.MethodPost, base+"/observations", f.admin, "case-rejected-key", reviewRequest(1, []PullInput{pull}))
	if advanced.Code != http.StatusCreated {
		t.Fatal("invalid import poisoned retry", advanced.Code, advanced.Body.String())
	}
	replay := f.call(http.MethodPost, base+"/observations", f.admin, "canonical-history", original)
	if replay.Code != http.StatusCreated || replay.Body.String() != receipt.Body.String() || countRows(t, f.store, "review_snapshots") != 2 || countRows(t, f.store, "events") != 2 || countRows(t, f.store, "receipts") != 2 {
		t.Fatal("canonical replay changed after advancement", replay.Code, replay.Body.String())
	}
}

func TestObservationRetainsUnknownEmptySHA(t *testing.T) {
	f := newAPIFixture(t)
	pull := samplePull(2, "Unknown source identities")
	pull.Runs = []RunInput{{SourceRunID: "unknown-run", Channel: "manual", Scope: "unknown comparison", Outcome: "unknown", Found: 1, Accepted: 1, Findings: []Finding{{SourceFindingID: "unknown-fix", Title: "Unknown fix identity", Severity: "unknown", Disposition: "accepted"}}}}
	pull.Attempts = []Attempt{{SourceAttemptID: "unknown-attempt", Channel: "manual", State: "unknown"}}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "empty-identities")
	base := "/api/v1/projects/" + f.project + "/reviews"
	for _, path := range []string{"/queue", "/pulls/2", "/pulls/2/runs", "/runs/" + runOpaqueID(f.project, "test-import", "unknown-run")} {
		response := f.call(http.MethodGet, base+path, f.readToken, "", nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"head_sha":""`) {
			t.Fatal("unknown identity unavailable", path, response.Code, response.Body.String())
		}
	}
}
