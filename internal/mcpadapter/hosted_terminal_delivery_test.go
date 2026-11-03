package mcpadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This proves delivery through the real native HTTP wait and SDK stdio session.
// It cannot establish when an external host presents an already returned result.
func TestHostedTerminalDeliveryAndAcknowledgementDuringLongSDKWait(t *testing.T) {
	f := mutationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	stop := time.AfterFunc(12*time.Second, cancel)
	t.Cleanup(func() { stop.Stop(); cancel() })
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema} {
		if err := ensure(ctx, f.store); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "hosted-delivery", Provider: "synthetic", Account: "fixture", Channels: []string{"hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := func(admin bool) core.Scope {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.SetPathValue("project_id", f.project)
		r.Header.Set("X-FC-Worker", f.sender)
		r.Header.Set("Authorization", "Bearer "+token)
		if admin {
			r.Header.Set("Authorization", "Bearer "+f.admin)
			value, err := f.store.AdminScope(r)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		value, err := f.store.Scope(r, "write")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	zero := int64(0)
	if _, err := engine.ConfigurePolicy(ctx, scope(true), "hosted-delivery-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Configure(ctx, scope(true), "hosted-delivery-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "hosted-delivery", Mode: "independent", Queue: []int64{2}, Channels: []string{"hosted"}}); err != nil {
		t.Fatal(err)
	}
	queued, err := engine.Enqueue(ctx, scope(false), "hosted-delivery-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(queued, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := engine.StartAttempt(ctx, f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := reviewexec.Anchor{RepositoryID: 1, Repository: "example/adapter", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), BaseRef: "main", PatchSHA256: strings.Repeat("d", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: receipt.ID, AttemptID: attempt.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = engine.Admit(ctx, f.project, attempt.ID, attempt.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = engine.Advance(ctx, f.project, attempt.ID, attempt.Revision, "active", "synthetic_trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	started := make(chan struct{}, 1)
	var operationReads, acknowledgements atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/"+f.project+"/reviews/operations/"+receipt.ID+"/status" {
			operationReads.Add(1)
			seconds, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Prefer"), "wait="))
			if err != nil || seconds < 890 || seconds > 900 || r.Method != http.MethodGet {
				t.Error("long SDK wait did not reach the real native wait", r.Header.Get("Prefer"))
			}
			started <- struct{}{}
		}
		if strings.HasSuffix(r.URL.Path, "/ack") {
			acknowledgements.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(service.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	serverSession, err := f.adapter(t, service.URL, f.sender).Connect(ctx, &mcp.IOTransport{Reader: inR, Writer: outW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "hosted-delivery-stdio", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		serverSession.Close()
		session.Close()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	})
	invoke := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		defer callCancel()
		value, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s did not return promptly: %v", name, err)
		}
		return value
	}
	// A self-addressed native message lets this same bound session acknowledge it.
	var message client.MessageReceipt
	decodeResult(t, invoke("inbox_send", map[string]any{"recipient_worker_id": f.sender, "body": "Acknowledge during Hosted wait", "idempotency_key": "hosted-delivery-message"}), &message)
	type result struct {
		value *mcp.CallToolResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_operation_read", Arguments: map[string]any{"operation_id": receipt.ID, "wait_seconds": 900}})
		done <- result{value, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("SDK wait never reached native HTTP")
	}
	var ack, replay client.MessageReceipt
	args := map[string]any{"message_id": message.ID, "idempotency_key": "hosted-delivery-ack"}
	decodeResult(t, invoke("inbox_ack", args), &ack)
	decodeResult(t, invoke("inbox_ack", args), &replay)
	if ack.Outcome != "acknowledged" || ack != replay || ack.ID != message.ID {
		t.Fatal("same-session acknowledgement or exact-key replay changed", ack, replay)
	}
	select {
	case value := <-done:
		t.Fatalf("pending Hosted wait returned before completion: %+v", value)
	case <-time.After(25 * time.Millisecond):
	}
	finishedAt := time.Now().UTC()
	digest := strings.Repeat("e", 64)
	capture, err := engine.RecordReviewEvidence(ctx, f.project, attempt.ID, reviewexec.ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: startedAt.Format(time.RFC3339Nano), FinishedAt: finishedAt.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReviewedFiles: []string{}, Findings: []reviewprovider.ParsedFinding{}}, Hosted: &reviewexec.HostedEvidenceMetadata{TriggerCommentID: 101, ReviewIDs: []int64{}, ObservedBaseSHA: anchor.ParentSHA, Sources: []reviewexec.HostedFindingSource{}, StandingSummary: &reviewexec.HostedStandingSummaryMetadata{CommentID: 501, ProviderRunID: "33333333-3333-3333-3333-333333333333", UpdatedAt: finishedAt.Format(time.RFC3339Nano)}}})
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now()
	if _, err := engine.Advance(ctx, f.project, attempt.ID, attempt.Revision, "completed", "synthetic_capture_complete", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case returned := <-done:
		if returned.err != nil {
			t.Fatal(returned.err)
		}
		var status client.ReviewOperationStatusOutput
		decodeResult(t, returned.value, &status)
		if status.ID != receipt.ID || status.State != "completed" || status.EvidenceID != capture.ID || status.NextAction != "read_evidence" {
			t.Fatal("terminal SDK delivery lost native evidence identity", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("committed Hosted completion did not wake the 900-second SDK wait promptly")
	}
	if operationReads.Load() != 1 || acknowledgements.Load() != 2 || f.providers.Load() != 1 {
		t.Fatal("unexpected repeated operation reads, acknowledgement calls or provider work", operationReads.Load(), acknowledgements.Load(), f.providers.Load())
	}
	t.Logf("real Hosted completion reached same SDK stdio session in %s; acknowledgement and exact-key replay completed while wait remained pending", time.Since(completedAt))
}
