package reviews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func transferRequest(revision int64, groups ...TransferGroup) TransferRequest {
	return TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 701, Publisher: "prepared-history-export", Groups: groups}
}
func transferPreviewFor(t *testing.T, f *apiFixture, req TransferRequest) TransferPreview {
	t.Helper()
	before := importDatabaseState(t, f)
	w := importHTTP(f, importBase(f)+"/transfer/preview", f.admin, "", "", importJSON(t, req))
	requireImportResponse(t, w, 200, "")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("preview changed database values")
	}
	var response TransferPreview
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.NativeCoverage || !response.ImportedHistory || response.Coverage != "partial" {
		t.Fatal("transfer claims unsupported authority/coverage")
	}
	return response
}
func transferApplyFor(t *testing.T, f *apiFixture, req TransferRequest, key string) (Receipt, TransferApplyRequest, string) {
	t.Helper()
	preview := transferPreviewFor(t, f, req)
	apply := TransferApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: req}
	w := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", key, importJSON(t, apply))
	requireImportResponse(t, w, 201, "")
	var receipt Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.Outcome != "transferred" {
		t.Fatal("invalid transfer receipt")
	}
	return receipt, apply, w.Body.String()
}

func TestReviewTransferMixedProducersAdditiveGrowthAndReplay(t *testing.T) {
	f := newAPIFixture(t)
	pull := fullImportRequest().Queue[0]
	pull.Complete = false
	first, apply, receipt := transferApplyFor(t, f, transferRequest(0, TransferGroup{Source: "producer-one", Pulls: []PullInput{pull}}, TransferGroup{Source: "producer-two", Pulls: []PullInput{pull}}), "mixed-first")
	old := f.call("GET", importBase(f)+"/runs/"+runOpaqueID(f.project, "producer-one", pull.Runs[0].SourceRunID)+"?snapshot="+first.ID, f.readToken, "", nil)
	requireImportResponse(t, old, 200, "")
	var snapshotSource string
	if err := f.store.DB().QueryRow(`SELECT source FROM review_snapshots WHERE snapshot_id=?`, first.ID).Scan(&snapshotSource); err != nil || snapshotSource != "prepared-history-export" {
		t.Fatal("publisher provenance lost", err)
	}
	var observed string
	if err := f.store.DB().QueryRow(`SELECT observed_at FROM review_snapshots WHERE snapshot_id=?`, first.ID).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	for batch := 0; batch < 3; batch++ {
		selected := pull
		selected.Runs = nil
		selected.Attempts = nil
		for i := 0; i < 75; i++ {
			run := pull.Runs[0]
			run.SourceRunID = fmt.Sprintf("growth-%d-%d", batch, i)
			selected.Runs = append(selected.Runs, run)
			attempt := pull.Attempts[0]
			attempt.SourceAttemptID = run.SourceRunID
			attempt.SourceRunID = run.SourceRunID
			selected.Attempts = append(selected.Attempts, attempt)
		}
		transferApplyFor(t, f, transferRequest(int64(batch+1), TransferGroup{Source: "producer-one", Pulls: []PullInput{selected}}), fmt.Sprintf("growth-%d", batch))
	}
	var runs, attempts int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_runs WHERE project_id=?`, f.project).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_attempts WHERE project_id=?`, f.project).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if runs != 227 || attempts != 227 {
		t.Fatal("history was dropped or lifetime-capped", runs, attempts)
	}
	var latestObserved string
	if err := f.store.DB().QueryRow(`SELECT s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.snapshot_id=w.snapshot_id AND s.project_id=w.project_id WHERE w.project_id=?`, f.project).Scan(&latestObserved); err != nil || latestObserved != observed {
		t.Fatal("partial import refreshed unrelated observations", err)
	}
	now := f.call("GET", importBase(f)+"/runs/"+runOpaqueID(f.project, "producer-one", pull.Runs[0].SourceRunID)+"?snapshot="+first.ID, f.readToken, "", nil)
	if now.Code != 200 || now.Body.String() != old.Body.String() {
		t.Fatal("additive import changed pinned historical bytes")
	}
	before := importDatabaseState(t, f)
	replay := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "mixed-first", importJSON(t, apply))
	if replay.Code != 201 || replay.Body.String() != receipt || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("exact replay did not preserve original receipt/state")
	}
	apply.Batch.Publisher = "changed-publisher"
	changedBody, err := normalizeTransfer(&apply.Batch)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := transferPreview(f.store.InstanceID(), transferTestScope(t, f), apply.Batch, changedBody)
	if err != nil {
		t.Fatal(err)
	}
	apply.PreviewSHA256 = preview.PreviewSHA256
	changed := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "mixed-first", importJSON(t, apply))
	requireImportResponse(t, changed, 409, "idempotency_conflict")
}

func transferTestScope(t *testing.T, f *apiFixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", importBase(f)+"/transfer/preview", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestReviewTransferPreviewConflictsAndFailureAreAtomic(t *testing.T) {
	f := newAPIFixture(t)
	pull := fullImportRequest().Queue[0]
	pull.Complete = false
	_, _, _ = transferApplyFor(t, f, transferRequest(0, TransferGroup{Source: "original", Pulls: []PullInput{pull}}), "seed")
	for _, tc := range []struct {
		name   string
		change func(*PullInput)
	}{
		{"metadata", func(p *PullInput) { p.Title = "different" }},
		{"count", func(p *PullInput) { p.Runs[0].Found++; p.Runs[0].Complete = false }},
		{"text", func(p *PullInput) { p.Runs[0].Findings[0].Title = "changed known finding" }},
		{"settled", func(p *PullInput) { p.Attempts[0].State = "active" }},
		{"linked-head", func(p *PullInput) { p.Attempts[0].HeadSHA = strings.Repeat("b", 40) }},
		{"missing-link", func(p *PullInput) { p.Attempts[0].SourceRunID = "not-retained" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected PullInput
			json.Unmarshal(importJSON(t, pull), &selected)
			tc.change(&selected)
			before := importDatabaseState(t, f)
			w := importHTTP(f, importBase(f)+"/transfer/preview", f.admin, "", "", importJSON(t, transferRequest(1, TransferGroup{Source: "original", Pulls: []PullInput{selected}})))
			requireImportResponse(t, w, 409, "observation_conflict")
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("failed preview changed state")
			}
		})
	}
	req := transferRequest(1, TransferGroup{Source: "another", Pulls: []PullInput{pull}})
	preview := transferPreviewFor(t, f, req)
	before := importDatabaseState(t, f)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER transfer_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	w := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "rollback", importJSON(t, TransferApplyRequest{preview.PreviewSHA256, req}))
	if w.Code != http.StatusInternalServerError {
		t.Fatal("injected event failure not refused", w.Code)
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("failed apply partially changed state")
	}
}
