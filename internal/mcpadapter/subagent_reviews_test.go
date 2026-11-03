package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
)

func subagentMCPRecord() map[string]any {
	return map[string]any{"idempotency_key": "record-once", "pull_number": 2, "head_sha": strings.Repeat("a", 40), "reviewer": "Independent Sol", "scope": "Database invariants", "started_at": "2026-10-15T00:00:00Z", "finished_at": "2026-10-15T00:01:00Z", "findings": []any{map[string]any{"title": "Check invariant", "detail": "Markdown **source proof** at `source.go:12`.", "severity": "high"}}}
}
func subagentMCPClone(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		t.Fatal("clone")
	}
	return out
}

func TestSubagentMCPDiscoveryAndInvalidBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	identity := localContext()
	identity.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"subagent": identity}, "missing-private-token"), "subagent")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range listed.Tools {
		if !strings.HasPrefix(tool.Name, "review_subagent_") {
			continue
		}
		found++
		write := tool.Name != "review_subagent_read"
		destructive := tool.Name == "review_subagent_update"
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint != destructive || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatal("subagent tool changed authority", tool.Name)
		}
		data, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(data), `"additionalProperties":false`) {
			t.Fatal("open schema", tool.Name)
		}
		if tool.Name == "review_subagent_record" {
			var schema struct {
				Properties map[string]struct {
					MaxLength   int    `json:"maxLength"`
					Description string `json:"description"`
				} `json:"properties"`
			}
			if json.Unmarshal(data, &schema) != nil {
				t.Fatal("invalid record schema")
			}
			for _, field := range []string{"reviewer", "scope"} {
				v := schema.Properties[field]
				if v.MaxLength != 128 || !strings.Contains(v.Description, "128 UTF-8 bytes") {
					t.Fatal("record discovery omitted label byte bound", field, v)
				}
			}
			if !strings.Contains(schema.Properties["scope"].Description, "coverage_limits") || !strings.Contains(tool.Description, "128 UTF-8 bytes") {
				t.Fatal("record discovery omitted short-scope guidance")
			}
		}
	}
	if found != 3 {
		t.Fatal("subagent discovery incomplete", found)
	}
	base := subagentMCPRecord()
	for _, missing := range []string{"idempotency_key", "pull_number", "head_sha", "reviewer", "scope", "started_at", "finished_at", "findings"} {
		bad := subagentMCPClone(t, base)
		delete(bad, missing)
		diagnosticResult(t, call(t, session, "review_subagent_record", bad), "invalid_arguments", false)
	}
	for _, field := range []string{"project_id", "worker_id", "channel", "provider", "source", "accepted", "unknown"} {
		bad := subagentMCPClone(t, base)
		bad[field] = "PRIVATE_SENTINEL"
		diagnosticResult(t, call(t, session, "review_subagent_record", bad), "invalid_arguments", false)
	}
	for _, finding := range []any{nil, map[string]any{"title": "T", "detail": "D", "severity": "high", "reason": nil}, map[string]any{"title": "T", "detail": "D", "severity": "high", "disposition": "accepted"}, map[string]any{"title": "T", "detail": "D", "severity": "major"}, map[string]any{"title": "T", "detail": strings.Repeat("x", 16385), "severity": "high"}} {
		bad := subagentMCPClone(t, base)
		bad["findings"] = []any{finding}
		diagnosticResult(t, call(t, session, "review_subagent_record", bad), "invalid_arguments", false)
	}
	for _, bad := range []map[string]any{
		{"run_id": "review_evidence_" + strings.Repeat("a", 32)},
		{"run_id": "review_run_" + strings.Repeat("a", 32), "worker_id": "other"},
	} {
		diagnosticResult(t, call(t, session, "review_subagent_read", bad), "invalid_arguments", false)
	}
	id := "review_run_" + strings.Repeat("a", 32)
	for _, bad := range []map[string]any{
		{"run_id": id, "idempotency_key": "update", "expected_revision": 1},
		{"run_id": id, "idempotency_key": "update", "expected_revision": 0, "finalize": true},
		{"run_id": id, "idempotency_key": "update", "expected_revision": 1, "finalize": nil},
		{"run_id": id, "idempotency_key": "update", "expected_revision": 1, "decisions": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "R"}, map[string]any{"finding": 0, "disposition": "rejected", "reason": "R"}}},
		{"run_id": id, "idempotency_key": "update", "expected_revision": 1, "fixes": []any{map[string]any{"finding": 0, "fix_sha": strings.Repeat("b", 40), "proof_note": "P"}}},
	} {
		diagnosticResult(t, call(t, session, "review_subagent_update", bad), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid arguments reached credentials or HTTP", requests.Load())
	}
}

func TestSubagentMCPActualWorkerRecordReplayDecisionsAndFix(t *testing.T) {
	f := mutationFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := f.handler.(*http.ServeMux)
	reviews.Register(mux, f.store)
	zero := int64(0)
	observation := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "controlled-subagent-fixture", ObservedAt: "2026-10-15T00:00:00Z", Queue: []reviews.PullInput{{Number: 2, Title: "Controlled PR", State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown"}}}
	body, _ := json.Marshal(observation)
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(body))
	setup.Header.Set("Authorization", "Bearer "+f.admin)
	setup.Header.Set("Idempotency-Key", "seed-workspace")
	seeded := httptest.NewRecorder()
	mux.ServeHTTP(seeded, setup)
	if seeded.Code != http.StatusCreated {
		t.Fatal("controlled workspace", seeded.Code, seeded.Body.String())
	}
	var drop atomic.Bool
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/subagent-reports") {
			writes.Add(1)
			if r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("subagent write lost binding")
			}
			if drop.Swap(false) {
				captured := httptest.NewRecorder()
				mux.ServeHTTP(captured, r)
				if captured.Code != http.StatusCreated {
					t.Error("lost-response action not committed", captured.Code, captured.Body.String())
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	record := subagentMCPRecord()
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_subagent_record", record), "connection_failed", true)
	var receipt, replay client.SubagentReceipt
	decodeResult(t, call(t, session, "review_subagent_record", record), &receipt)
	decodeResult(t, call(t, session, "review_subagent_record", record), &replay)
	if receipt != replay || receipt.State != "recorded" || receipt.Revision < 1 || !strings.HasPrefix(receipt.ID, "review_run_") {
		t.Fatal("record replay changed", receipt, replay)
	}
	var report client.SubagentReport
	decodeResult(t, call(t, session, "review_subagent_read", map[string]any{"run_id": receipt.ID}), &report)
	if report.ID != receipt.ID || report.ProjectID != f.project || report.Revision != receipt.Revision || report.Run.Channel != "subagent" || report.Run.Finalized || report.Run.Found != 1 || len(report.Findings) != 1 || report.Findings[0].Disposition != "unadjudicated" {
		t.Fatal("recorded report lost source", report)
	}
	// A bad later selected index must not apply the earlier decision.
	badBatch := map[string]any{"run_id": receipt.ID, "idempotency_key": "atomic-refusal", "expected_revision": report.Revision, "decisions": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Verified source invariant"}, map[string]any{"finding": 99, "disposition": "accepted", "reason": "Outside source inventory"}}}
	diagnosticResult(t, call(t, session, "review_subagent_update", badBatch), "validation_failed", false)
	var unchanged client.SubagentReport
	decodeResult(t, call(t, session, "review_subagent_read", map[string]any{"run_id": receipt.ID}), &unchanged)
	if unchanged.Revision != report.Revision || unchanged.Findings[0].Disposition != "unadjudicated" {
		t.Fatal("rejected batch partly applied", unchanged)
	}
	update := map[string]any{"run_id": receipt.ID, "idempotency_key": "decide-finalize", "expected_revision": report.Revision, "decisions": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Verified source invariant"}}, "finalize": true}
	decodeResult(t, call(t, session, "review_subagent_update", update), &receipt)
	decodeResult(t, call(t, session, "review_subagent_update", update), &replay)
	if receipt != replay || receipt.State != "updated" {
		t.Fatal("update replay changed", receipt, replay)
	}
	fix := map[string]any{"run_id": receipt.ID, "idempotency_key": "fix-source", "expected_revision": receipt.Revision, "fixes": []any{map[string]any{"finding": 0, "expected_sha": "", "fix_sha": strings.Repeat("c", 40), "proof_note": "Immutable focused proof"}}}
	decodeResult(t, call(t, session, "review_subagent_update", fix), &receipt)
	decodeResult(t, call(t, session, "review_subagent_read", map[string]any{"run_id": receipt.ID}), &report)
	if report.Revision != receipt.Revision || !report.Run.Finalized || report.Run.Accepted != 1 || report.Run.Fixed != 1 || report.Findings[0].FixSHA != strings.Repeat("c", 40) {
		t.Fatal("source counts/fix lost", report)
	}
	var records, executionTables int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_runs WHERE project_id=?`, f.project).Scan(&records); err != nil || records != 1 {
		t.Fatal("duplicate review", records, err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='review_operations'`).Scan(&executionTables); err != nil || executionTables != 0 {
		t.Fatal("subagent report installed provider execution", executionTables, err)
	}
	// A complete clean review can be recorded/finalized in one ordinary call.
	clean := subagentMCPRecord()
	clean["idempotency_key"] = "clean-review"
	clean["findings"] = []any{}
	clean["finalized"] = true
	decodeResult(t, call(t, session, "review_subagent_record", clean), &receipt)
	decodeResult(t, call(t, session, "review_subagent_read", map[string]any{"run_id": receipt.ID}), &report)
	if !report.Run.Finalized || report.Run.Found != 0 || report.Findings == nil || len(report.Findings) != 0 {
		t.Fatal("clean review was not finalized in one record call", report)
	}
	if f.providers.Load() != 1 || writes.Load() != 8 {
		t.Fatal("unexpected provider work or automatic HTTP retry", f.providers.Load(), writes.Load())
	}
}

func TestSubagentMCPLabelByteBoundsAndDiagnostics(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	identity := localContext()
	identity.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"subagent": identity}, "missing-private-token"), "subagent")
	for _, field := range []string{"reviewer", "scope"} {
		for _, tc := range []struct {
			name, value string
			valid       bool
		}{
			{"ASCII boundary", strings.Repeat("x", 128), true},
			{"UTF8 boundary", strings.Repeat("é", 64), true},
			{"mixed UTF8 boundary", strings.Repeat("雪", 42) + "é", true},
			{"ASCII over", strings.Repeat("x", 129), false},
			{"UTF8 over", strings.Repeat("雪", 43), false},
			{"mixed UTF8 over", strings.Repeat("é", 64) + "x", false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				args := subagentMCPRecord()
				args[field] = tc.value
				result := call(t, session, "review_subagent_record", args)
				if tc.valid {
					// Reaching credential admission proves the exact 128-byte label
					// passed parsing; no service or retained write is needed.
					diagnosticResult(t, result, "credential_unreadable", false)
					return
				}
				d := diagnosticResult(t, result, "invalid_arguments", false)
				if !strings.Contains(d.Message, field) || !strings.Contains(d.Action, "128") || !strings.Contains(d.Action, "bytes") || !strings.Contains(d.Action, "No request was sent") || strings.Contains(d.Message, tc.value) || strings.Contains(d.Action, tc.value) || strings.Contains(d.Action, "binding") {
					t.Fatal("label validation lost its safe field-specific byte diagnostic", d)
				}
				if field == "scope" && !strings.Contains(d.Action, "coverage_limits") {
					t.Fatal("scope diagnostic omitted the supported longer-text field", d)
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatal("label validation contacted HTTP", requests.Load())
	}
}
