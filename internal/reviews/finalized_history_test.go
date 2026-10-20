package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Install actual version-one columns. No current-schema trigger/view is
	// dropped and no runtime compatibility writer is introduced.
	f := newAPIFixtureWithSchema(t, func(ctx context.Context, store *core.Store) error {
		return store.EnsureSchema(ctx, "reviews", reviewSchemaMigrations()[:1]...)
	})
	pull := partialHistoryPull()
	original := reviewRequest(0, []PullInput{pull, samplePull(9, "Retained merged history")})
	normalized, err := normalizeObservation(&original)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(normalized)
	request := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", nil)
	request.SetPathValue("project_id", f.project)
	request.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.AdminScope(request)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := core.NewID("review_snapshot")
	recorded := time.Now().UTC().Format(time.RFC3339Nano)
	saved := Receipt{ID: snapshot, Revision: 1, Outcome: "observed"}
	result, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "reviews.observe", Target: "workspace", Key: "original-key", Request: original}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots VALUES(?,?,1,701,?,?,?,?)`, f.project, snapshot, original.Source, original.ObservedAt, recorded, hex.EncodeToString(fingerprint[:])); err != nil {
			return nil, err
		}
		for position, item := range original.Queue {
			recent := []RunSummary{}
			for i := len(item.Runs) - 1; i >= 0 && len(recent) < 3; i-- {
				recent = append(recent, summarizeRun(f.project, original.Source, item.Number, item.Runs[i]))
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_pulls VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.project, snapshot, position, item.Number, item.Title, item.State, item.HeadSHA, item.BaseRef, item.BaseSHA, item.CIState, item.Mergeability, item.Complete, len(item.Runs), 0, mustJSON(recent), "[]"); err != nil {
				return nil, err
			}
			for order, run := range item.Runs {
				summary := summarizeRun(f.project, original.Source, item.Number, run)
				if _, err := tx.ExecContext(ctx, `INSERT INTO review_runs VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.project, snapshot, item.Number, order, summary.ID, original.Source, run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete, run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, summary.Fixed, mustJSON(run.CoverageLimits)); err != nil {
					return nil, err
				}
				for index, finding := range run.Findings {
					if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.project, snapshot, item.Number, summary.ID, index, finding.SourceFindingID, finding.Title, finding.Detail, finding.Severity, finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt, finding.FixSHA, finding.ProofNote, finding.ResolvedAt); err != nil {
						return nil, err
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_workspace VALUES(?,1,?)`, f.project, snapshot); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "review.observed", snapshot, 1, map[string]any{"snapshot_id": snapshot, "queue_count": len(original.Queue)}); err != nil {
			return nil, err
		}
		return saved, nil
	})
	if err != nil {
		t.Fatal("old fixture mutation", err)
	}
	receipt := httptest.NewRecorder()
	core.WriteJSON(receipt, 201, json.RawMessage(result))
	expected := RunDetailResponse{ProjectID: f.project, Revision: 1, SnapshotID: snapshot, Source: original.Source, ObservedAt: original.ObservedAt, RecordedAt: recorded, PullNumber: pull.Number, Run: summarizeRun(f.project, original.Source, pull.Number, original.Queue[0].Runs[0]), Findings: original.Queue[0].Runs[0].Findings}
	oldEncoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	oldBody := string(append(oldEncoded, '\n'))
	firstPage := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/queue?limit=1", f.readToken, "", nil)
	var page QueueResponse
	if firstPage.Code != 200 || json.Unmarshal(firstPage.Body.Bytes(), &page) != nil || !page.HasMore || page.Cursor == "" {
		t.Fatal("old schema cursor unavailable")
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
	if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "8" {
		t.Fatal("migration version", version, err)
	}
	if err := store.DB().QueryRow(`SELECT snapshot_revision FROM review_runs WHERE project_id=? AND snapshot_id=?`, f.project, saved.ID).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("old finalized row not backfilled", revision, err)
	}
	if historyReadBody(t, f, saved.ID) != oldBody {
		t.Fatal("migration changed retained facts")
	}
	continued := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/queue?limit=1&cursor="+url.QueryEscape(page.Cursor), f.readToken, "", nil)
	var tail QueueResponse
	if continued.Code != 200 || json.Unmarshal(continued.Body.Bytes(), &tail) != nil || tail.SnapshotID != saved.ID || len(tail.Items) != 1 || tail.Items[0].Number != 9 {
		t.Fatal("migration changed old signed continuation", continued.Code)
	}
	before := [3]int{countRows(t, store, "review_snapshots"), countRows(t, store, "events"), countRows(t, store, "receipts")}
	replay := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "original-key", original)
	if replay.Code != 201 || replay.Body.String() != receipt.Body.String() || before != ([3]int{countRows(t, store, "review_snapshots"), countRows(t, store, "events"), countRows(t, store, "receipts")}) {
		t.Fatal("migration changed exact replay", replay.Code, replay.Body.String())
	}
	pull.Runs[0].Findings[0].Title = "Changed old source"
	response := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "changed-after-migration", reviewRequest(1, []PullInput{pull, samplePull(9, "Retained merged history")}))
	if response.Code != 409 {
		t.Fatal("migration lost old source authority", response.Code, response.Body.String())
	}
	if _, err := store.DB().Exec(`UPDATE review_run_versions SET version_revision=999`); err == nil {
		t.Fatal("owning revision update guard absent")
	}
	columns := "project_id,id,version_revision," + strings.Join(historyRunRecord.body, ",")
	if _, err := store.DB().Exec("INSERT INTO review_run_versions(" + columns + ") SELECT project_id,id||'-other',999," + strings.Join(historyRunRecord.body, ",") + " FROM review_run_versions LIMIT 1"); err == nil {
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
	if !strings.Contains(plan, "USING INDEX review_run_versions_source_revision") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN review_run_versions") {
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
