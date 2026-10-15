package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func partialHistoryPull() PullInput {
	pull := samplePull(8, "Retained partial history")
	pull.Runs = []RunInput{{SourceRunID: "partial-history", Channel: "manual", Scope: "bounded", Outcome: "unknown", Found: 2, Accepted: 1, Finalized: true, Findings: []Finding{{SourceFindingID: "first", Title: "First immutable title", Detail: "First immutable detail", Severity: "medium", Disposition: "accepted"}}}}
	return pull
}

func historyReadBody(t *testing.T, f *apiFixture, snapshot string) string {
	t.Helper()
	response := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/runs/"+runOpaqueID(f.project, "test-import", "partial-history")+"?snapshot="+snapshot, f.readToken, "", nil)
	if response.Code != 200 {
		t.Fatal("historical detail unavailable", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func TestFinalizedHistoryPreservesEnrichmentAcrossOmittedRunGaps(t *testing.T) {
	f := newAPIFixture(t)
	pull := partialHistoryPull()
	first := f.observe(t, reviewRequest(0, []PullInput{pull}), "history-first")["id"].(string)
	firstBody := historyReadBody(t, f, first)
	// Omitting a run does not erase earlier finalized source authority.
	empty := pull
	empty.Runs = nil
	f.observe(t, reviewRequest(1, []PullInput{empty}), "history-gap")
	pull.Runs[0].Findings = append(pull.Runs[0].Findings, Finding{SourceFindingID: "second", Title: "Second immutable title", Detail: "Second immutable detail", Severity: "low", Disposition: "rejected"})
	added := f.observe(t, reviewRequest(2, []PullInput{pull}), "history-enrichment")["id"].(string)
	addedBody := historyReadBody(t, f, added)
	f.observe(t, reviewRequest(3, []PullInput{empty}), "history-second-gap")
	for _, test := range []struct {
		name   string
		change func(*PullInput)
	}{
		{"remove-added-finding", func(p *PullInput) { p.Runs[0].Findings = p.Runs[0].Findings[:1] }},
		{"replace-old-text", func(p *PullInput) { p.Runs[0].Findings[0].Detail = "Replacement old detail" }},
		{"replace-new-text", func(p *PullInput) { p.Runs[0].Findings[1].Title = "Replacement added title" }},
		{"change-count", func(p *PullInput) { p.Runs[0].Found++ }},
		{"change-identity", func(p *PullInput) { p.Runs[0].Scope = "other comparison" }},
		{"unfinalize", func(p *PullInput) { p.Runs[0].Finalized = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var changed PullInput
			data, _ := json.Marshal(pull)
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			test.change(&changed)
			before := [3]int{countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events"), countRows(t, f.store, "receipts")}
			response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, test.name, reviewRequest(4, []PullInput{changed}))
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "observation_conflict") {
				t.Fatal("historical source invariant lost", response.Code, response.Body.String())
			}
			if before != ([3]int{countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events"), countRows(t, f.store, "receipts")}) {
				t.Fatal("historical conflict partially committed")
			}
		})
	}
	pull.Runs[0].Findings[0].ProofNote = "Current accepted proof correction"
	f.observe(t, reviewRequest(4, []PullInput{pull}), "history-proof-correction")
	if historyReadBody(t, f, first) != firstBody || historyReadBody(t, f, added) != addedBody {
		t.Fatal("new observations rewrote historical detail")
	}
	var mismatches int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_runs r JOIN review_snapshots s ON s.project_id=r.project_id AND s.snapshot_id=r.snapshot_id WHERE r.snapshot_revision!=s.revision`).Scan(&mismatches); err != nil || mismatches != 0 {
		t.Fatal("new run did not retain owning revision", mismatches, err)
	}
}

func TestFinalizedHistoryVersionOneMigrationAndReplay(t *testing.T) {
	f := newAPIFixture(t)
	pull := partialHistoryPull()
	original := reviewRequest(0, []PullInput{pull})
	receipt := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "original-key", original)
	if receipt.Code != 201 {
		t.Fatal(receipt.Body.String())
	}
	var saved Receipt
	if err := json.Unmarshal(receipt.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	oldBody := historyReadBody(t, f, saved.ID)
	// Reconstruct exactly the old schema only in this owned fixture.
	for _, statement := range []string{
		`DROP TRIGGER review_runs_snapshot_revision_insert`,
		`DROP TRIGGER review_runs_snapshot_revision_update`,
		`DROP INDEX review_runs_project_source_id`,
		`ALTER TABLE review_runs DROP COLUMN snapshot_revision`,
		`ALTER TABLE review_findings DROP COLUMN route_json`,
		`CREATE INDEX review_runs_project_source_id ON review_runs(project_id,source,source_run_id,finalized)`,
		`UPDATE metadata SET value='1' WHERE key='schema:reviews'`,
	} {
		if _, err := f.store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var sequence int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	if err := EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(t.Context(), store); err != nil {
		t.Fatal("migration was not idempotent", err)
	}
	mux := http.NewServeMux()
	Register(mux, store)
	f.mux = mux
	var version string
	var revision int64
	if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "3" {
		t.Fatal("migration version", version, err)
	}
	if err := store.DB().QueryRow(`SELECT snapshot_revision FROM review_runs WHERE project_id=? AND snapshot_id=?`, f.project, saved.ID).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("old finalized row not backfilled", revision, err)
	}
	if historyReadBody(t, f, saved.ID) != oldBody {
		t.Fatal("migration changed retained facts")
	}
	before := [3]int{countRows(t, store, "review_snapshots"), countRows(t, store, "events"), countRows(t, store, "receipts")}
	replay := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "original-key", original)
	if replay.Code != 201 || replay.Body.String() != receipt.Body.String() || before != ([3]int{countRows(t, store, "review_snapshots"), countRows(t, store, "events"), countRows(t, store, "receipts")}) {
		t.Fatal("migration changed exact replay", replay.Code, replay.Body.String())
	}
	pull.Runs[0].Findings[0].Title = "Changed old source"
	response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "changed-after-migration", reviewRequest(1, []PullInput{pull}))
	if response.Code != 409 {
		t.Fatal("migration lost old source authority", response.Code, response.Body.String())
	}
	if _, err := store.DB().Exec(`UPDATE review_runs SET snapshot_revision=0`); err == nil {
		t.Fatal("owning revision update guard absent")
	}
	if _, err := store.DB().Exec(`INSERT INTO review_runs SELECT project_id,snapshot_id,pull_number,run_order+1,id||'-other',source,source_run_id||'-other',channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json,0 FROM review_runs LIMIT 1`); err == nil {
		t.Fatal("owning revision insert guard absent")
	}
}

func TestFinalizedHistoryLookupUsesOrderedSourceIndex(t *testing.T) {
	f := newAPIFixture(t)
	pull := partialHistoryPull()
	f.observe(t, reviewRequest(0, []PullInput{pull}), "query-plan-source")
	rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+finalizedRunQuery, f.project, "test-import", pull.Runs[0].SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "USING INDEX review_runs_project_source_id") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN review_runs") {
		t.Fatal("finalized lookup scans or sorts history", plan)
	}
	// The selected source can be read with a bounded read-only transaction.
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := validateFinalizedCounts(context.Background(), tx, f.project, "test-import", []PullInput{pull}); err != nil {
		t.Fatal(err)
	}
}
