package mcpadapter_test

import (
	"bytes"
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
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reviewControlsStdio(t *testing.T, path, name string) *mcp.ClientSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan int, 1)
	go func() {
		done <- mcpadapter.Run(ctx, []string{"--profile", name, "--profiles-file", path}, nil, inR, outW, &stderr)
		outW.Close()
	}()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "review-controls-stdio-fixture", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		inW.Close()
		outR.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Close()
		select {
		case code := <-done:
			if code != 0 || stderr.Len() != 0 {
				t.Error("stdio session did not exit cleanly")
			}
		case <-time.After(time.Second):
			t.Error("stdio session leaked")
		}
		cancel()
		inW.Close()
		outR.Close()
	})
	return session
}

func reviewControlsFixture(t *testing.T) (*mutationAPI, http.Handler, string) {
	t.Helper()
	f := mutationFixture(t)
	ctx := context.Background()
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema} {
		if err := ensure(ctx, f.store); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "fixture", Provider: "synthetic", Account: "fixture", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := func(admin bool) core.Scope {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.SetPathValue("project_id", f.project)
		r.Header.Set("X-FC-Worker", f.sender)
		var result core.Scope
		var err error
		if admin {
			r.Header.Set("Authorization", "Bearer "+f.admin)
			result, err = f.store.AdminScope(r)
		} else {
			r.Header.Set("Authorization", "Bearer "+token)
			result, err = f.store.Scope(r, "write")
		}
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	zero := int64(0)
	if _, err = engine.ConfigurePolicy(ctx, scope(true), "fixture-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli", "hosted"}, MaxReviewFiles: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Configure(ctx, scope(true), "fixture-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "fixture", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	queued, err := engine.Enqueue(ctx, scope(false), "fixture-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(queued, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := engine.StartAttempt(ctx, f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	anchor := reviewexec.Anchor{RepositoryID: 1, Repository: "example/adapter", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), BaseRef: "main", PatchSHA256: strings.Repeat("d", 64), ObservedAt: now.Format(time.RFC3339Nano)}
	attempt, err = engine.Admit(ctx, f.project, attempt.ID, attempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("e", 64)
	capture, err := engine.RecordReviewEvidence(ctx, f.project, attempt.ID, reviewexec.ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: now.Add(-time.Second).Format(time.RFC3339Nano), FinishedAt: now.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 1, ReviewedFiles: []string{"changed.go"}, Findings: []reviewprovider.ParsedFinding{{Title: "Immutable source", Detail: "PRIVATE_FINDING_TEXT <script>literal</script>", Severity: "high", Path: "changed.go", StartLine: 1, EndLine: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Advance(ctx, f.project, attempt.ID, attempt.Revision, "completed", "fixture_capture", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	return f, mux, capture.ID
}

func reviewControlsProfile(t *testing.T, f *mutationAPI, server string) string {
	t.Helper()
	c := profile.Context{Profile: "chosen", ServerURL: server, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	return profiles(t, map[string]profile.Context{"chosen": c}, f.credential)
}

func TestReviewControlsStdioSchemasAndLocalContext(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "missing-private-credential")
	session := reviewControlsStdio(t, path, "alpha")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"review_evidence_read": {"evidence_id"}, "review_adjudication_read": {"evidence_id"},
		"review_finding_decide":  {"evidence_id", "idempotency_key", "expected_revision", "finding", "disposition", "reason"},
		"review_finding_fix":     {"evidence_id", "idempotency_key", "expected_revision", "finding", "expected_sha", "fix_sha", "proof_note"},
		"review_source_finalize": {"evidence_id", "idempotency_key", "expected_revision"},
	}
	for _, tool := range listed.Tools {
		required, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
			Additional bool                      `json:"additionalProperties"`
		}
		data, _ := json.Marshal(tool.InputSchema)
		expectedProperties := len(required)
		if tool.Name == "review_finding_decide" {
			expectedProperties++
		}
		if json.Unmarshal(data, &schema) != nil || schema.Additional || len(schema.Properties) != expectedProperties || strings.Join(schema.Required, ",") != strings.Join(required, ",") {
			t.Fatal("review control schema changed required keys or permits overrides")
		}
		if finding := schema.Properties["finding"]; finding != nil && (finding["minimum"] != float64(0) || finding["maximum"] != float64(99) || !strings.Contains(finding["description"].(string), "zero-based")) {
			t.Fatal("finding index does not advertise zero-based bounds")
		}
		write := tool.Name == "review_finding_decide" || tool.Name == "review_finding_fix" || tool.Name == "review_source_finalize"
		destructive := tool.Name == "review_finding_decide" || tool.Name == "review_finding_fix"
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint != destructive || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatal("review control annotations do not match effects")
		}
	}
	if len(want) != 0 {
		t.Fatal("review control discovery incomplete")
	}
	var got profile.Context
	decodeResult(t, call(t, session, "context", nil), &got)
	if got != c || requests.Load() != 0 {
		t.Fatal("discovery/context performed credential or network work")
	}
}

func TestReviewControlsStdioReadsDecisionsFixFinalizationAndExactRetry(t *testing.T) {
	f, handler, evidenceID := reviewControlsFixture(t)
	var writes, reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews/") {
			if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.sender || r.URL.RawQuery != "" {
				t.Error("review request lost immutable binding or introduced selectors")
			}
			if r.Method == http.MethodPost {
				writes.Add(1)
				if r.Header.Get("Idempotency-Key") == "" {
					t.Error("review mutation omitted explicit key")
				}
			} else {
				reads.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	var evidence client.ReviewEvidence
	decodeResult(t, call(t, session, "review_evidence_read", map[string]any{"evidence_id": evidenceID}), &evidence)
	if evidence.ID != evidenceID || len(evidence.Capture.Findings) != 1 || evidence.Capture.Findings[0].Finding != 0 || !strings.Contains(evidence.Capture.Findings[0].Detail, "PRIVATE_FINDING_TEXT") {
		t.Fatal("explicit immutable evidence read lost source text/index")
	}
	var initial client.ReviewAdjudication
	initialResult := call(t, session, "review_adjudication_read", map[string]any{"evidence_id": evidenceID})
	decodeResult(t, initialResult, &initial)
	encoded, _ := json.Marshal(initialResult.StructuredContent)
	if initial.Revision != 0 || initial.Finalized || initial.FrozenCounts != nil || len(initial.Findings) != 1 || initial.Findings[0].Disposition != "unadjudicated" || strings.Contains(string(encoded), "PRIVATE_FINDING_TEXT") {
		t.Fatal("compact adjudication read expanded source or fabricated decisions")
	}
	decision := map[string]any{"evidence_id": evidenceID, "idempotency_key": "decision-once", "expected_revision": 0, "finding": 0, "disposition": "accepted", "reason": "Explicit decision\nwith source proof"}
	var receipt, replay client.ReviewControlReceipt
	decodeResult(t, call(t, session, "review_finding_decide", decision), &receipt)
	decodeResult(t, call(t, session, "review_finding_decide", decision), &replay)
	if receipt != replay || receipt.ID != evidenceID || receipt.Revision != 1 || receipt.State != "adjudicated" {
		t.Fatal("zero revision/index or exact decision retry changed")
	}
	decision["reason"] = "Changed request"
	diagnosticResult(t, call(t, session, "review_finding_decide", decision), "idempotency_conflict", false)
	fix := map[string]any{"evidence_id": evidenceID, "idempotency_key": "fix-once", "expected_revision": 1, "finding": 0, "expected_sha": "", "fix_sha": strings.Repeat("f", 40), "proof_note": "Focused local proof"}
	decodeResult(t, call(t, session, "review_finding_fix", fix), &receipt)
	decodeResult(t, call(t, session, "review_finding_fix", fix), &replay)
	if receipt != replay || receipt.Revision != 2 || receipt.State != "fix_recorded" {
		t.Fatal("explicit empty expected SHA or exact fix retry changed")
	}
	finalize := map[string]any{"evidence_id": evidenceID, "idempotency_key": "finalize-once", "expected_revision": 2}
	decodeResult(t, call(t, session, "review_source_finalize", finalize), &receipt)
	decodeResult(t, call(t, session, "review_source_finalize", finalize), &replay)
	if receipt != replay || receipt.Revision != 3 || receipt.State != "finalized" {
		t.Fatal("finalization receipt or exact retry changed")
	}
	compact, _ := json.Marshal(receipt)
	var fields map[string]any
	_ = json.Unmarshal(compact, &fields)
	if len(fields) != 3 || len(compact) > 200 {
		t.Fatal("mutation receipt expanded source or proof")
	}
	var final client.ReviewAdjudication
	decodeResult(t, call(t, session, "review_adjudication_read", map[string]any{"evidence_id": evidenceID}), &final)
	if final.Revision != 3 || !final.Finalized || final.FrozenCounts == nil || final.FrozenCounts.Found != 1 || final.FrozenCounts.Accepted != 1 || final.Findings[0].FixSHA != strings.Repeat("f", 40) || final.Findings[0].ProofNote != "Focused local proof" {
		t.Fatal("final source counts or effective proof changed")
	}
	var changes int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?", evidenceID).Scan(&changes); err != nil || changes != 3 || writes.Load() != 7 || reads.Load() != 3 || f.providers.Load() != 1 {
		t.Fatal("retry duplicated durable changes or tools performed extra provider work")
	}
}

func TestReviewControlsArgumentsRejectBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "absent-private-credential")
	session := reviewControlsStdio(t, path, "alpha")
	id := "review_evidence_" + strings.Repeat("a", 32)
	base := map[string]map[string]any{
		"review_evidence_read":     {"evidence_id": id},
		"review_adjudication_read": {"evidence_id": id},
		"review_finding_decide":    {"evidence_id": id, "idempotency_key": "explicit", "expected_revision": 0, "finding": 0, "disposition": "accepted", "reason": "source proof"},
		"review_finding_fix":       {"evidence_id": id, "idempotency_key": "explicit", "expected_revision": 0, "finding": 0, "expected_sha": "", "fix_sha": strings.Repeat("f", 40), "proof_note": "fix proof"},
		"review_source_finalize":   {"evidence_id": id, "idempotency_key": "explicit", "expected_revision": 0},
	}
	for name, valid := range base {
		t.Run(name, func(t *testing.T) {
			check := func(value any) {
				t.Helper()
				diagnosticResult(t, call(t, session, name, value), "invalid_arguments", false)
			}
			check(json.RawMessage(`null`))
			check(json.RawMessage(`[]`))
			for field := range valid {
				copy := make(map[string]any)
				for key, value := range valid {
					if key != field {
						copy[key] = value
					}
				}
				check(copy)
				copy[field] = nil
				check(copy)
			}
			for _, bad := range []struct {
				field  string
				values []any
			}{
				{"evidence_id", []any{"../escape", "review_evidence_" + strings.Repeat("A", 32), "review_run_" + strings.Repeat("a", 32), "bad\x00", 7}},
				{"idempotency_key", []any{"", " padded", "key\n", strings.Repeat("k", 129), "é"}},
				{"expected_revision", []any{-1, 0.5, "0", json.Number("9223372036854775807"), json.Number("9223372036854775808")}},
				{"finding", []any{-1, 100, 0.5, "0"}},
				{"disposition", []any{"routed", "Accepted", ""}},
				{"reason", []any{"", " \n", "bad\x00", "bad\u0085", strings.Repeat("é", 1025), 9}},
				{"proof_note", []any{"", " \n", "bad\x1b", strings.Repeat("é", 1025), 9}},
				{"expected_sha", []any{strings.Repeat("A", 40), strings.Repeat("a", 39), 9}},
				{"fix_sha", []any{"", strings.Repeat("A", 40), strings.Repeat("f", 41)}},
			} {
				if _, present := valid[bad.field]; !present {
					continue
				}
				for _, value := range bad.values {
					copy := make(map[string]any)
					for key, original := range valid {
						copy[key] = original
					}
					copy[bad.field] = value
					check(copy)
				}
			}
			data, _ := json.Marshal(valid)
			check(json.RawMessage(strings.TrimSuffix(string(data), "}") + `,"evidence_id":"` + id + `"}`))
			check(json.RawMessage(strings.Replace(string(data), `"evidence_id"`, `"Evidence_id"`, 1)))
			check(json.RawMessage(strings.Replace(string(data), id, `bad\ud800`, 1)))
			for _, field := range []string{"profile", "project_id", "worker_id", "server_url", "capture"} {
				copy := make(map[string]any)
				for key, value := range valid {
					copy[key] = value
				}
				copy[field] = "override"
				check(copy)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid review-control arguments read credentials or reached HTTP")
	}
}

func TestReviewControlsUnknownOutcomeRequiresExplicitSameKeyRetry(t *testing.T) {
	for _, mode := range []string{"dropped-response", "malformed-receipt"} {
		t.Run(mode, func(t *testing.T) {
			f, handler, id := reviewControlsFixture(t)
			var writes atomic.Int64
			var first atomic.Bool
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/decisions") {
					writes.Add(1)
					if first.CompareAndSwap(false, true) {
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, r)
						if rec.Code != http.StatusOK {
							t.Error("fixture decision did not commit")
							return
						}
						if mode == "dropped-response" {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{"id":"invalid"}`))
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer service.Close()
			session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
			args := map[string]any{"evidence_id": id, "idempotency_key": "retain-this-key", "expected_revision": 0, "finding": 0, "disposition": "accepted", "reason": "Explicit source proof"}
			code := "connection_failed"
			if mode == "malformed-receipt" {
				code = "invalid_response"
			}
			failure := diagnosticResult(t, call(t, session, "review_finding_decide", args), code, true)
			if writes.Load() != 1 || !strings.Contains(failure.Action, "same idempotency key") {
				t.Fatal("mutation retried automatically or recovery guidance lost")
			}
			var receipt client.ReviewControlReceipt
			decodeResult(t, call(t, session, "review_finding_decide", args), &receipt)
			var changes int
			if err := f.store.DB().QueryRow("SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?", id).Scan(&changes); err != nil || changes != 1 || writes.Load() != 2 || receipt.Revision != 1 {
				t.Fatal("explicit retry duplicated decision")
			}
		})
	}
}

func TestReviewControlsCanceledContextStopsBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	s := adapter(t, profiles(t, map[string]profile.Context{"alpha": c}, "missing-private-credential"), "alpha")
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return next(canceled, method, request)
			}
			return next(ctx, method, request)
		}
	})
	session := connect(t, s, "")
	id := "review_evidence_" + strings.Repeat("a", 32)
	for i, name := range []string{"review_evidence_read", "review_adjudication_read", "review_finding_decide", "review_finding_fix", "review_source_finalize"} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			args := map[string]any{"evidence_id": id}
			if strings.Contains(name, "finding") || name == "review_source_finalize" {
				args["idempotency_key"] = "key"
				args["expected_revision"] = 0
			}
			if name == "review_finding_decide" {
				args["finding"] = 0
				args["disposition"] = "accepted"
				args["reason"] = "proof"
			}
			if name == "review_finding_fix" {
				args["finding"] = 0
				args["expected_sha"] = ""
				args["fix_sha"] = strings.Repeat("f", 40)
				args["proof_note"] = "proof"
			}
			diagnosticResult(t, call(t, session, name, args), "canceled", false)
		})
	}
	if requests.Load() != 0 {
		t.Fatal("canceled operation opened credential or contacted HTTP")
	}
}
