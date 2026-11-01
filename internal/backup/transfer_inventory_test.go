package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestTransferInventoryMixedBatchesSurviveInactiveRestore(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	prefix := "/api/v1/projects/" + f.project + "/reviews"
	read := func(handler http.Handler, store *core.Store, token, route string) []byte {
		t.Helper()
		r := httptest.NewRequest("GET", prefix+route, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Instance", store.InstanceID())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("read %s: %d %s", route, w.Code, w.Body.String())
		}
		return bytes.Clone(w.Body.Bytes())
	}
	var first string
	var pinned map[string][]byte
	for batch := 0; batch < 3; batch++ {
		revision := int64(batch)
		req := reviews.TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 991, Publisher: "synthetic-transfer-export"}
		for _, source := range []string{"historical-cli", "historical-hosted"} {
			pull := reviews.PullInput{Number: 11, Title: "Merged transfer history", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: false}
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("shared-%d-%d", batch, i)
				run := reviews.RunInput{SourceRunID: id, Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Historical reviewer", Scope: "Retained historical comparison", Outcome: "failed", CoverageLimits: []string{"Original capture incomplete"}, Findings: []reviews.Finding{}}
				attempt := reviews.Attempt{SourceAttemptID: id, Channel: "cli", HeadSHA: pull.HeadSHA, State: "failed", SourceRunID: id}
				if i == 0 {
					run.Outcome, run.Complete, run.Attributable, run.Finalized = "completed", true, true, true
					run.StartedAt, run.FinishedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z"
					attempt.State, attempt.StartedAt, attempt.FinishedAt = "completed", run.StartedAt, run.FinishedAt
				}
				pull.Runs = append(pull.Runs, run)
				pull.Attempts = append(pull.Attempts, attempt)
			}
			req.Groups = append(req.Groups, reviews.TransferGroup{Source: source, Pulls: []reviews.PullInput{pull}})
		}
		beforePreview := backupRows(t, f.store.DB())
		previewMap := f.request(t, "POST", prefix+"/transfer/preview", req)
		if !reflect.DeepEqual(beforePreview, backupRows(t, f.store.DB())) {
			t.Fatal("transfer preview changed source rows")
		}
		preview := reviews.TransferPreview{}
		body, err := json.Marshal(previewMap)
		if err != nil || json.Unmarshal(body, &preview) != nil || preview.Counts.Runs != 80 || preview.Counts.Attempts != 80 || preview.NativeCoverage || !preview.ImportedHistory {
			t.Fatal("invalid transfer preview", err)
		}
		receipt := f.request(t, "POST", prefix+"/transfer/apply", reviews.TransferApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: req})
		if receipt["outcome"] != "transferred" {
			t.Fatal("missing real transfer receipt")
		}
		if batch == 0 {
			first = receipt["id"].(string)
			var runs reviews.RunsResponse
			if err := json.Unmarshal(read(f.handler, f.store, f.token, "/pulls/11/runs?limit=100&snapshot="+first), &runs); err != nil || len(runs.Items) != 80 || runs.HasMore {
				t.Fatal("first bounded history unavailable", err)
			}
			pinned = map[string][]byte{}
			for _, route := range []string{"/pulls/11?snapshot=" + first, "/pulls/11/runs?limit=100&snapshot=" + first, "/pulls/11/attempts?limit=100&snapshot=" + first, "/runs/" + runs.Items[0].ID + "?snapshot=" + first} {
				pinned[route] = read(f.handler, f.store, f.token, route)
			}
		}
	}
	var current reviews.PullResponse
	if err := json.Unmarshal(read(f.handler, f.store, f.token, "/pulls/11"), &current); err != nil || current.Pull.RunCount != 240 || current.Pull.AttemptCount != 240 || current.Pull.Complete {
		t.Fatal("additive history did not grow beyond former lifetime caps", err, current)
	}
	for _, table := range []string{"review_run_versions", "review_attempt_versions"} {
		var known, unknown int
		if err := f.store.DB().QueryRow("SELECT count(*),sum(CASE WHEN started_at='' AND finished_at='' THEN 1 ELSE 0 END) FROM "+table+" WHERE project_id=?", f.project).Scan(&known, &unknown); err != nil || known != 240 || unknown != 234 {
			t.Fatal("unknown historical times were invented or history omitted", table, known, unknown, err)
		}
		for _, source := range []string{"historical-cli", "historical-hosted"} {
			var count int
			if err := f.store.DB().QueryRow("SELECT count(*) FROM "+table+" WHERE project_id=? AND source=?", f.project, source).Scan(&count); err != nil || count != 120 {
				t.Fatal("original producer identity lost", table, source, count, err)
			}
		}
	}
	for route, body := range pinned {
		if !bytes.Equal(body, read(f.handler, f.store, f.token, route)) {
			t.Fatal("later batches changed pinned response", route)
		}
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	files := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("create/inspect changed source rows or schema")
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" {
		t.Fatal("inactive restore failed", restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	retained := backupRows(t, db)
	db.Close()
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && retained[table] != rows {
			t.Fatalf("inactive restore changed retained %s", table)
		}
	}
	// Explicit activation permits API proof with fresh credentials. Historical
	// pages here have no continuation cursor, so exact response bytes survive
	// the intentional restore identity/signing-key rotation.
	token := "transfer-backup-fresh-activation-token-1234567890"
	tokenFile := filepath.Join(t.TempDir(), "fresh.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(t.Context(), destination, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: tokenFile}); err != nil {
		t.Fatal(err)
	}
	reopened := activationOpen(t, destination)
	mux := http.NewServeMux()
	reviews.Register(mux, reopened)
	for route, body := range pinned {
		if !bytes.Equal(body, read(mux, reopened, token, route)) {
			t.Fatal("restored pinned response bytes changed", route)
		}
	}
	if !reflect.DeepEqual(files, restoreTree(t, bundle.Path)) {
		t.Fatal("restore changed immutable source bundle")
	}
}
