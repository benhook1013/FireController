package reviewexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Use the actual administrator boundary so every derived reference/index and
// partial-coverage claim comes from canonical admission, not synthetic SQL rows.
func transferProjectionBatch(t *testing.T, f *attemptFixture, key string, groups []reviews.TransferGroup) reviews.Receipt {
	t.Helper()
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	req := reviews.TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 801, Publisher: "growth-transfer", Groups: groups}
	path := "/api/v1/projects/" + attemptProject + "/reviews/transfer"
	before := transferAuthorityRows(t, f, true)
	w := f.request("POST", path+"/preview", f.admin, "", encoded(req))
	evidenceHTTPRequire(t, w, 200, "")
	var preview reviews.TransferPreview
	if json.Unmarshal(w.Body.Bytes(), &preview) != nil || !preview.ImportedHistory || preview.NativeCoverage || preview.Coverage != "partial" || !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
		t.Fatal("growth preview changed state or authority")
	}
	w = f.request("POST", path+"/apply", f.admin, key, encoded(reviews.TransferApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: req}))
	evidenceHTTPRequire(t, w, 201, "")
	var receipt reviews.Receipt
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.Revision != revision+1 || receipt.Outcome != "transferred" {
		t.Fatal("growth transfer did not commit compact result")
	}
	return receipt
}

func TestNativeExecutionAfterTransferredHistoryGrowth(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	publicsite.New(f.store, reviews.PublicSiteReviewFacts).Register(mux, t.Context())
	f.handler = mux
	publicationPath := "/api/v1/projects/" + attemptProject + "/publication"
	// Existing executor fixtures allow operator attribution without a worker;
	// public configuration deliberately requires an explicit real worker.
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'growth-worker','Growth publisher')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	publicationCall := func(method, key, body string) *httptest.ResponseRecorder {
		path := publicationPath
		if method == "POST" {
			path += "/preview"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("X-FC-Worker", "growth-worker")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	approved := `{"expected_revision":0,"enabled":true,"manifest":{"slug":"native-growth","presentation":{"name":"Native growth"},"reviews":[{"number":2,"title":"Approved growing PR","url":"https://github.com/owner/repo/pull/2"}]}}`
	w := publicationCall("POST", "", approved)
	evidenceHTTPRequire(t, w, 200, "")
	var preview publicsite.Preview
	if json.Unmarshal(w.Body.Bytes(), &preview) != nil {
		t.Fatal("public growth preview invalid")
	}
	var apply map[string]any
	_ = json.Unmarshal([]byte(approved), &apply)
	apply["preview_sha256"] = preview.SHA256
	evidenceHTTPRequire(t, publicationCall("PUT", "growth-public", encoded(apply)), 200, "")
	publicRead := func(path string) (publicsite.Snapshot, []byte) {
		t.Helper()
		w := f.request("GET", path, "", "", "")
		evidenceHTTPRequire(t, w, 200, "")
		var snapshot publicsite.Snapshot
		if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil {
			t.Fatal("public growth snapshot invalid")
		}
		return snapshot, bytes.Clone(w.Body.Bytes())
	}
	firstPublic, firstPublicBytes := publicRead("/api/v1/public/projects/native-growth")
	authority := transferAuthorityRows(t, f, false)
	var old reviews.PullResponse
	var oldRuns, oldAttempts, oldNextRuns, oldNextAttempts []byte
	var oldRunPage reviews.RunsResponse
	var oldAttemptPage reviews.AttemptsResponse
	for batch := 0; batch < 3; batch++ {
		count := 50
		if batch == 2 {
			count = 1
		}
		var groups []reviews.TransferGroup
		for producer, channel := range []string{"cli", "hosted"} {
			pull := projectionPull(2)
			pull.Complete = false
			for n := batch * 50; n < batch*50+count; n++ {
				run := reviews.RunInput{SourceRunID: fmt.Sprintf("shared-growth-run-%03d", n), Channel: channel, HeadSHA: pull.HeadSHA, Scope: "prepared selected history", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00.123456789Z", Found: 6}
				for finding := 0; finding < 6; finding++ {
					run.Findings = append(run.Findings, reviews.Finding{SourceFindingID: fmt.Sprintf("f-%d", finding), Title: "Retained source finding", Severity: "low", Disposition: "rejected"})
				}
				pull.Runs = append(pull.Runs, run)
				pull.Attempts = append(pull.Attempts, reviews.Attempt{SourceAttemptID: fmt.Sprintf("shared-growth-attempt-%03d", n), Channel: channel, HeadSHA: pull.HeadSHA, State: "unknown", SourceRunID: run.SourceRunID})
			}
			groups = append(groups, reviews.TransferGroup{Source: fmt.Sprintf("original-producer-%d", producer), Pulls: []reviews.PullInput{pull}})
		}
		transferProjectionBatch(t, f, fmt.Sprintf("growth-batch-%d", batch), groups)
		if batch == 0 {
			projectionRead(t, f, "/pulls/2", &old)
			oldRuns = projectionRead(t, f, "/pulls/2/runs?snapshot="+old.SnapshotID, &oldRunPage)
			oldAttempts = projectionRead(t, f, "/pulls/2/attempts?snapshot="+old.SnapshotID, &oldAttemptPage)
			var nextRuns reviews.RunsResponse
			var nextAttempts reviews.AttemptsResponse
			oldNextRuns = projectionRead(t, f, "/pulls/2/runs?snapshot="+old.SnapshotID+"&cursor="+oldRunPage.Cursor, &nextRuns)
			oldNextAttempts = projectionRead(t, f, "/pulls/2/attempts?snapshot="+old.SnapshotID+"&cursor="+oldAttemptPage.Cursor, &nextAttempts)
		}
	}
	if !reflect.DeepEqual(authority, transferAuthorityRows(t, f, false)) {
		t.Fatal("transferred history manufactured executor/capture/adjudication authority")
	}
	var imported reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &imported)
	if imported.Pull.RunCount != 202 || imported.Pull.AttemptCount != 202 || imported.Pull.Complete || attemptCount(t, f.store, `SELECT count(*) FROM review_findings WHERE project_id=? AND snapshot_id=?`, attemptProject, imported.SnapshotID) != 1212 {
		t.Fatal("canonical transferred growth fixture incomplete", imported.Pull.RunCount, imported.Pull.AttemptCount)
	}
	limited, limitedBytes := publicRead("/api/v1/public/projects/native-growth")
	if len(limited.Content.Reviews) != 1 || len(limited.Content.Reviews[0].CLI) != 100 || len(limited.Content.Reviews[0].Hosted) != 100 || !limited.Content.Reviews[0].CLIHasMore || !limited.Content.Reviews[0].HostedHasMore {
		t.Fatal("coupled public retained growth did not stay bounded/honest")
	}
	current := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}
	covered, err := f.e.Coverage(t.Context(), attemptProject, current, "cli")
	if err != nil || covered {
		t.Fatal("imported counts became native comparison authority", covered, err)
	}
	var captured Receipt
	var captureInput ReviewEvidenceInput
	var activeAttempt Attempt
	driver := &controlledDriver{}
	driver.prepare = func(ctx context.Context, _ Operation, _ Configuration, a Attempt) (Preparation, error) {
		if _, body := publicRead("/api/v1/public/projects/native-growth"); !bytes.Equal(body, limitedBytes) {
			t.Fatal("private preparing attempt changed limited publication")
		}
		anchor := attemptAnchor(attemptProject, 2)
		anchor.ParentSHA = anchor.BaseSHA
		return Preparation{Anchor: anchor}, nil
	}
	driver.dispatch = func(ctx context.Context, _ Operation, a Attempt, _ func(context.Context, string) error) (Result, error) {
		activeAttempt = a
		var capture bytes.Buffer
		encoder := json.NewEncoder(&capture)
		for _, event := range []any{map[string]any{"type": "review_context", "reviewType": "committed", "currentBranch": "HEAD", "baseBranch": "origin/main", "workingDirectory": "/controlled/growth"}, map[string]any{"type": "finding", "title": "Actual controlled native finding", "comment": "Literal captured source finding", "severity": "major", "fileName": "internal/changed.go", "startLine": 1, "endLine": 1}, map[string]any{"type": "complete", "status": "review_completed", "outcome": "completed", "findings": 1, "reviewedFiles": []string{"internal/changed.go"}}} {
			if err := encoder.Encode(event); err != nil {
				return Result{}, err
			}
		}
		parsed, err := reviewprovider.ParseCLIReview(capture.Bytes(), 0, reviewprovider.CLIContext{BaseBranch: "origin/main", WorkingDirectory: "/controlled/growth"})
		if err != nil {
			return Result{}, err
		}
		digest := sha256.Sum256(capture.Bytes())
		hexDigest := hex.EncodeToString(digest[:])
		captureInput = ReviewEvidenceInput{CaptureID: "review_capture_" + hexDigest[:32], CaptureSHA256: hexDigest, CaptureBytes: int64(capture.Len()), StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: *a.Anchor, Review: parsed}
		captured, err = f.e.RecordReviewEvidence(ctx, attemptProject, a.ID, captureInput)
		if err != nil {
			return Result{}, err
		}
		if _, err = f.e.PublishEvidence(ctx, attemptProject, captured.ID, nil); err != nil {
			return Result{}, err
		}
		if _, body := publicRead("/api/v1/public/projects/native-growth"); !bytes.Equal(body, limitedBytes) {
			t.Fatal("unfinalized captured source changed limited publication")
		}
		return Result{State: "completed", Reason: "controlled_capture_retained"}, nil
	}
	request := f.enqueue(t, attemptProject, "native-after-transferred-growth", "cli", 2, 1)
	if err = f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal("native work after growth", err)
	}
	operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
	if err != nil || operation.State != "completed" || driver.prepares.Load() != 1 || driver.calls.Load() != 1 || captured.ID == "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("growth changed genuine native execution/capture exactly-once boundaries", operation, err)
	}
	var native reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &native)
	if native.Pull.RunCount != 203 || native.Pull.AttemptCount != 203 {
		t.Fatal("native source/attempt not appended after transferred totals")
	}
	_, projected := projectedAttempt(t, f, activeAttempt)
	if projected.State != "completed" || projected.SourceRunID != captured.ID {
		t.Fatal("native attempt/capture link not retained")
	}
	adjudicationDecide(t, f, captured.ID, "growth-source-reject", 0, 0, "rejected")
	if _, body := publicRead("/api/v1/public/projects/native-growth"); !bytes.Equal(body, limitedBytes) {
		t.Fatal("unfinalized source decision changed public freshness")
	}
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "growth-source-finalize", captured.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, captured.ID, "finalized", 2)
	covered, err = f.e.Coverage(t.Context(), attemptProject, current, "cli")
	if err != nil || !covered {
		t.Fatal("genuine completed/finalized native comparison absent after growth", covered, err)
	}
	detail := cliPublicationDetail(t, f, 2, captured.ID)
	if detail.Run.Found != 1 || detail.Run.Accepted != 0 || detail.Run.Routed != 0 || !detail.Run.Finalized || len(detail.Findings) != 1 || detail.Findings[0].Disposition != "rejected" {
		t.Fatal("source adjudication/native counts corrupted after growth")
	}
	afterPublic, _ := publicRead("/api/v1/public/projects/native-growth")
	if afterPublic.Revision != limited.Revision+1 || len(afterPublic.Content.Reviews[0].CLI) != 100 || len(afterPublic.Content.Reviews[0].Hosted) != 100 || !afterPublic.Content.Reviews[0].CLIHasMore || !afterPublic.Content.Reviews[0].HostedHasMore {
		t.Fatal("eligible native finalization lost bounded public history")
	}
	_, historicalBytes := publicRead(fmt.Sprintf("/api/v1/public/projects/native-growth/history/1?generation=%s", firstPublic.Generation))
	if !bytes.Equal(firstPublicBytes, historicalBytes) {
		t.Fatal("native growth rewrote frozen public revision")
	}
	var oldAfter reviews.PullResponse
	projectionRead(t, f, "/pulls/2?snapshot="+old.SnapshotID, &oldAfter)
	if !reflect.DeepEqual(old, oldAfter) {
		t.Fatal("native growth rewrote old compact PR")
	}
	var runs reviews.RunsResponse
	var attempts reviews.AttemptsResponse
	if !bytes.Equal(oldRuns, projectionRead(t, f, "/pulls/2/runs?snapshot="+old.SnapshotID, &runs)) || !bytes.Equal(oldAttempts, projectionRead(t, f, "/pulls/2/attempts?snapshot="+old.SnapshotID, &attempts)) || !bytes.Equal(oldNextRuns, projectionRead(t, f, "/pulls/2/runs?snapshot="+old.SnapshotID+"&cursor="+oldRunPage.Cursor, &runs)) || !bytes.Equal(oldNextAttempts, projectionRead(t, f, "/pulls/2/attempts?snapshot="+old.SnapshotID+"&cursor="+oldAttemptPage.Cursor, &attempts)) {
		t.Fatal("native growth rewrote pinned archive bytes/cursors")
	}
	for _, attempt := range projectionAttempts(t, f, 2, native.SnapshotID) {
		if strings.HasPrefix(attempt.Source, "original-producer-") && (attempt.StartedAt != "" || attempt.FinishedAt != "" || attempt.DurationSeconds != nil) {
			t.Fatal("native growth fabricated imported unknown times")
		}
	}
	beforeReplay := transferAuthorityRows(t, f, true)
	if replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, activeAttempt.ID, captureInput); err != nil || replay != captured {
		t.Fatal("exact captured input no longer replayed", err)
	}
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeReplay, transferAuthorityRows(t, f, true)) || driver.prepares.Load() != 1 || driver.calls.Load() != 1 {
		t.Fatal("exact native replay changed journal/history/receipts or provider count")
	}
}
