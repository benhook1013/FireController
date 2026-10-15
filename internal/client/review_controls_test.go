package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const controlEvidenceID = "review_evidence_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sampleControlEvidence() client.ReviewEvidence {
	timestamp := "2026-10-15T00:00:00Z"
	hash := strings.Repeat("a", 64)
	return client.ReviewEvidence{ID: controlEvidenceID, ProjectID: "project-one", AttemptID: "review_attempt_" + strings.Repeat("a", 32), OperationID: "review_operation_" + strings.Repeat("a", 32), CapabilityFingerprint: hash,
		Anchor:  client.ReviewAnchor{RepositoryID: 1, Repository: "owner/repo", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40), PatchSHA256: hash, BaseRef: "main", ObservedAt: timestamp},
		Capture: client.ReviewEvidenceCapture{CaptureID: "review_capture_" + hash[:32], CaptureSHA256: hash, CaptureBytes: 1024, StartedAt: timestamp, FinishedAt: timestamp, Outcome: "completed", Complete: true, ReportedFindings: 1, ReviewedFiles: []string{"source.go"}, Findings: []client.ReviewEvidenceFinding{{Finding: 0, Title: "Exact <finding>", Detail: "  immutable detail\n", Severity: "high", Path: "outside/diff.go", StartLine: 8, EndLine: 12}}}, RecordedAt: timestamp}
}

func TestReviewControlsNativeHTTPReceiptsReplayAndReadIsolation(t *testing.T) {
	f := newMutationFixture(t)
	ctx := context.Background()
	if err := reviewexec.EnsureSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviewexec.EnsureEvidenceSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviewexec.EnsureAdjudicationSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "test-cli", Provider: "coderabbit", Account: "synthetic", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", f.sender)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	_, err = e.ConfigurePolicy(ctx, scope, "seed-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli", "hosted"}, PausedChannels: []string{}, MaxReviewFiles: 0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Configure(ctx, scope, "seed-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "test-cli", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(ctx, scope, "seed-operation", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var operation reviewexec.Receipt
	if json.Unmarshal(raw, &operation) != nil {
		t.Fatal("bad seed receipt")
	}
	attempt, err := e.StartAttempt(ctx, f.project, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	anchor := reviewexec.Anchor{RepositoryID: 1, Repository: "owner/repository", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40), PatchSHA256: strings.Repeat("d", 64), BaseRef: "main", ObservedAt: now}
	attempt, err = e.Admit(ctx, f.project, attempt.ID, attempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("e", 64)
	capture, err := e.RecordReviewEvidence(ctx, f.project, attempt.ID, reviewexec.ReviewEvidenceInput{CaptureID: "review_capture_" + hash[:32], CaptureSHA256: hash, CaptureBytes: 1024, ExpectedAnchor: anchor, StartedAt: now, FinishedAt: now, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 1, ReviewedFiles: []string{"source.go"}, Findings: []reviewprovider.ParsedFinding{{Title: "Native source", Detail: "SOURCE_TEXT_NO_ECHO", Severity: "high"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(ctx, f.project, attempt.ID, attempt.Revision, "completed", "synthetic_completion", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	e.Register(mux)
	var drop atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("lost instance/worker binding")
		}
		if r.Method == "POST" && drop.Swap(false) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, r)
			if recorder.Code != 200 {
				t.Errorf("lost-response mutation did not commit: %s", recorder.Body.String())
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, err := client.New(ctx, profile.Binding{Context: profile.Context{Profile: "test", ServerURL: srv.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}, CredentialFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	counts := func() [2]int {
		var out [2]int
		for i, table := range []string{"events", "receipts"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := counts()
	provider := f.providerCalls.Load()
	evidence, err := c.ReviewEvidence(ctx, capture.ID)
	if err != nil || evidence.Capture.Findings[0].Finding != 0 || evidence.Capture.Findings[0].Detail != "SOURCE_TEXT_NO_ECHO" {
		t.Fatalf("native evidence read: %+v %v", evidence, err)
	}
	state, err := c.ReviewAdjudication(ctx, capture.ID)
	if err != nil || state.Revision != 0 || state.FrozenCounts != nil {
		t.Fatalf("initial null projection: %+v %v", state, err)
	}
	if counts() != before {
		t.Fatal("reads mutated journal")
	}
	decision := client.ReviewDecisionRequest{ExpectedRevision: 0, Finding: 0, Disposition: "accepted", Reason: "  exact reason\n"}
	drop.Store(true)
	callBefore := calls.Load()
	_, err = c.DecideReviewFinding(ctx, "decide-once", capture.ID, decision)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != callBefore+1 {
		t.Fatalf("lost outcome not safe/single-shot: %v", err)
	}
	state, err = c.ReviewAdjudication(ctx, capture.ID)
	if err != nil || state.Revision != 1 {
		t.Fatal("committed lost-response decision unavailable")
	}
	one := counts()
	receipt, err := c.DecideReviewFinding(ctx, "decide-once", capture.ID, decision)
	if err != nil || receipt.State != "adjudicated" || receipt.Revision != 1 || counts() != one {
		t.Fatalf("same-key recovery changed effect: %+v %v", receipt, err)
	}
	changed := decision
	changed.Reason = "different"
	_, err = c.DecideReviewFinding(ctx, "decide-once", capture.ID, changed)
	checkError(t, err, "idempotency_conflict")
	fix, err := c.RecordReviewFix(ctx, "fix-once", capture.ID, client.ReviewFixRequest{ExpectedRevision: 1, Finding: 0, ExpectedSHA: "", FixSHA: strings.Repeat("f", 40), ProofNote: "private focused proof"})
	if err != nil || fix.State != "fix_recorded" || fix.Revision != 2 {
		t.Fatalf("fix: %+v %v", fix, err)
	}
	final, err := c.FinalizeReviewSource(ctx, "finalize-once", capture.ID, client.ReviewFinalizeRequest{ExpectedRevision: 2})
	if err != nil || final.State != "finalized" || final.Revision != 3 {
		t.Fatalf("finalize: %+v %v", final, err)
	}
	state, err = c.ReviewAdjudication(ctx, capture.ID)
	if err != nil || !state.Finalized || state.FrozenCounts == nil || state.FrozenCounts.Accepted != 1 || state.Findings[0].ProofNote != "private focused proof" {
		t.Fatalf("finalized counts/proof: %+v %v", state, err)
	}
	if f.providerCalls.Load() != provider {
		t.Fatal("evidence controls contacted provider")
	}
	if _, err = f.store.DB().Exec("UPDATE credentials SET revoked=1 WHERE is_admin=1"); err != nil {
		t.Fatal(err)
	}
	_, err = c.ReviewEvidence(ctx, capture.ID)
	checkError(t, err, "unauthorized")
}

func TestReviewControlsStrictReadShapesAndBudgets(t *testing.T) {
	valid, _ := json.Marshal(sampleControlEvidence())
	for _, body := range []string{
		strings.Replace(string(valid), `"project_id":"project-one"`, `"project_id":"other"`, 1),
		strings.Replace(string(valid), `"finding":0`, `"finding":1`, 1),
		strings.Replace(string(valid), `"title":"Exact \u003cfinding\u003e"`, `"title":null`, 1),
		strings.Replace(string(valid), `"finding":0`, `"finding":0,"Finding":1`, 1),
		strings.Replace(string(valid), `"detail":"  immutable detail\n"`, `"detail":"\ud800"`, 1),
		strings.Replace(string(valid), `"detail":"  immutable detail\n"`, `"detail":"`+testToken+`"`, 1),
		strings.Replace(string(valid), `"reported_findings":1`, `"reported_findings":0`, 1),
		strings.Replace(string(valid), `"reviewed_files":["source.go"]`, `"reviewed_files":null`, 1),
		strings.Replace(string(valid), `"reviewed_files":["source.go"]`, `"reviewed_files":["source.go","source.go"]`, 1),
		strings.Replace(string(valid), `"path":"outside/diff.go"`, `"path":"../escape"`, 1),
		strings.Replace(string(valid), `"end_line":12`, `"end_line":7`, 1),
		strings.Replace(string(valid), `"complete":true`, `"complete":null`, 1),
		strings.Replace(string(valid), `"capture_bytes":1024`, `"capture_bytes":0`, 1),
		string(valid) + `{}`, strings.Repeat(" ", 4<<20) + string(valid),
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, srv)
		_, err := c.ReviewEvidence(context.Background(), controlEvidenceID)
		checkError(t, err, "invalid_response")
		c.Close()
		srv.Close()
	}
	base := client.ReviewAdjudication{EvidenceID: controlEvidenceID, Findings: []client.ReviewFindingDecision{{Finding: 0, Disposition: "unadjudicated"}}}
	raw, _ := json.Marshal(base)
	for _, body := range []string{strings.Replace(string(raw), `"frozen_counts":null,`, "", 1), strings.Replace(string(raw), `"revision":0`, `"revision":null`, 1), strings.Replace(string(raw), `"frozen_counts":null`, `"frozen_counts":{"found":1,"accepted":1,"routed":0}`, 1), strings.Replace(string(raw), `"finding":0`, `"finding":1`, 1), strings.Replace(string(raw), `"findings":[`, `"Findings":[`, 1)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, srv)
		_, err := c.ReviewAdjudication(context.Background(), controlEvidenceID)
		checkError(t, err, "invalid_response")
		c.Close()
		srv.Close()
	}
}

func TestReviewControlsInputAdmissionAndUnknownReceipts(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		write(w, 200, map[string]any{"id": controlEvidenceID, "state": "adjudicated", "revision": 2})
	}))
	defer srv.Close()
	c := newClient(t, srv)
	ctx := context.Background()
	for _, id := range []string{"", "../escape", "review_evidence_" + strings.Repeat("A", 32)} {
		_, err := c.ReviewEvidence(ctx, id)
		checkError(t, err, "validation_failed")
	}
	_, err := c.DecideReviewFinding(ctx, "key", controlEvidenceID, client.ReviewDecisionRequest{Finding: 100, Disposition: "accepted", Reason: "reason"})
	checkError(t, err, "validation_failed")
	_, err = c.RecordReviewFix(ctx, "key", controlEvidenceID, client.ReviewFixRequest{FixSHA: strings.Repeat("A", 40), ProofNote: "proof"})
	checkError(t, err, "validation_failed")
	_, err = c.FinalizeReviewSource(ctx, "key", controlEvidenceID, client.ReviewFinalizeRequest{ExpectedRevision: -1})
	checkError(t, err, "validation_failed")
	_, err = c.FinalizeReviewSource(ctx, " padded ", controlEvidenceID, client.ReviewFinalizeRequest{})
	checkError(t, err, "validation_failed")
	if calls.Load() != 0 {
		t.Fatal("invalid control reached HTTP")
	}
	_, err = c.DecideReviewFinding(ctx, "key", controlEvidenceID, client.ReviewDecisionRequest{Disposition: "accepted", Reason: "reason"})
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != 1 {
		t.Fatal("invalid mutation receipt must be unknown without retry")
	}
}

func TestReviewControlsStreamingErrorBoundAndLargerSuccess(t *testing.T) {
	var completed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(strings.Repeat(" ", 65<<10)))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		completed.Add(1)
	}))
	c := newClient(t, srv)
	started := time.Now()
	_, err := c.ReviewEvidence(context.Background(), controlEvidenceID)
	checkError(t, err, "invalid_response")
	if time.Since(started) > time.Second {
		t.Fatal("client waited for oversized streaming error body to finish")
	}
	c.Close()
	srv.Close()
	if completed.Load() != 1 {
		t.Fatal("streaming error handler did not finish")
	}
	large := sampleControlEvidence()
	large.Capture.Findings = nil
	for i := int64(0); i < 5; i++ {
		large.Capture.Findings = append(large.Capture.Findings, client.ReviewEvidenceFinding{Finding: i, Title: "bounded finding", Detail: strings.Repeat("x", 16<<10), Severity: "info"})
	}
	large.Capture.ReportedFindings = 5
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, large)
	}))
	defer success.Close()
	read := newClient(t, success)
	value, err := read.ReviewEvidence(context.Background(), controlEvidenceID)
	if err != nil || len(value.Capture.Findings) != 5 {
		t.Fatal("success budget was incorrectly reduced to error budget", err)
	}
}
