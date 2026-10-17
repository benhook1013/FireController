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
)

func TestReviewRoutesMCPActualPublicLostResponseRecoveryAndReceiverProof(t *testing.T) {
	f, handler, evidence := reviewControlsFixture(t)
	config := []byte(`{"expected_revision":1,"capability_id":"fixture","mode":"independent","queue":[2,4,6],"channels":["cli"],"paused":false}`)
	request := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/execution/configuration", bytes.NewReader(config))
	request.Header.Set("Authorization", "Bearer "+f.admin)
	request.Header.Set("Idempotency-Key", "route-receiver-config")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != 201 {
		t.Fatal("receiver config", rec.Code, rec.Body.String())
	}
	var posts atomic.Int64
	var drop atomic.Bool
	drop.Store(true)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("route SDK lost private profile binding")
		}
		if r.Method == "POST" {
			posts.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/routes") && r.Method == "POST" && drop.Swap(false) {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code != 200 {
				t.Error("route did not commit before lost response", recorded.Code, recorded.Body.String())
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	providers := f.providers.Load()
	args := map[string]any{"evidence_id": evidence, "idempotency_key": "route-once", "expected_revision": 0, "finding": 0, "target_pull_number": 4, "reason": "Exact retained routing reason"}
	diagnosticResult(t, call(t, session, "review_finding_route", args), "connection_failed", true)
	if posts.Load() != 1 {
		t.Fatal("lost route response retried automatically")
	}
	var receipt, replay client.ReviewRouteReceipt
	result := call(t, session, "review_finding_route", args)
	decodeResult(t, result, &receipt)
	decodeResult(t, call(t, session, "review_finding_route", args), &replay)
	data, _ := json.Marshal(result.StructuredContent)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if receipt != replay || receipt.State != "routed" || receipt.Revision != 1 || receipt.SourceRevision != 1 || len(fields) != 4 || strings.Contains(string(data), "routing reason") {
		t.Fatal("SDK route receipt expanded or replay changed", receipt, replay)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_finding_routes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("lost response duplicated route", count, err)
	}
	var page client.ReviewRoutePage
	decodeResult(t, call(t, session, "review_routes_list", map[string]any{"pull_number": 4}), &page)
	if len(page.Items) != 1 || page.Items[0].ID != receipt.ID || page.HasMore {
		t.Fatal("SDK incoming route list", page)
	}
	var detail client.ReviewRouteDetail
	decodeResult(t, call(t, session, "review_route_read", map[string]any{"route_id": receipt.ID}), &detail)
	if detail.SourceFinding.Finding != 0 || detail.SourceFinding.Detail != "PRIVATE_FINDING_TEXT <script>literal</script>" || detail.Route.TargetPullNumber != 4 {
		t.Fatal("SDK explicit original finding lost")
	}
	var control client.ReviewControlReceipt
	decodeResult(t, call(t, session, "review_source_finalize", map[string]any{"evidence_id": evidence, "idempotency_key": "route-finalize", "expected_revision": 1}), &control)
	decodeResult(t, call(t, session, "review_route_decide", map[string]any{"route_id": receipt.ID, "idempotency_key": "route-accept", "expected_revision": 1, "disposition": "accepted", "reason": "Receiving source verified"}), &control)
	decodeResult(t, call(t, session, "review_route_fix", map[string]any{"route_id": receipt.ID, "idempotency_key": "route-proof", "expected_revision": 2, "expected_sha": "", "fix_sha": strings.Repeat("f", 40), "proof_note": "Independent receiver proof"}), &control)
	var state client.ReviewAdjudication
	decodeResult(t, call(t, session, "review_adjudication_read", map[string]any{"evidence_id": evidence}), &state)
	if !state.Finalized || state.FrozenCounts == nil || *state.FrozenCounts != (client.ReviewCounts{Found: 1, Routed: 1}) || state.Findings[0].Disposition != "routed" || state.Findings[0].RouteID != receipt.ID || state.Findings[0].FixSHA != "" {
		t.Fatal("receiver correction changed frozen source counts/proof", state)
	}
	decodeResult(t, call(t, session, "review_route_read", map[string]any{"route_id": receipt.ID}), &detail)
	if detail.Route.FixSHA != strings.Repeat("f", 40) || detail.Route.ProofNote != "Independent receiver proof" || detail.Route.Revision != 3 {
		t.Fatal("receiver proof not readable", detail.Route)
	}
	decodeResult(t, call(t, session, "review_routes_list", map[string]any{"pull_number": 4, "limit": 1}), &page)
	if !page.Items[0].HasFix || page.Items[0].Disposition != "accepted" {
		t.Fatal("compact route summary lost effective receiver proof")
	}
	if f.providers.Load() != providers {
		t.Fatal("routing invoked provider")
	}
}

func TestReviewRoutesMCPDiscoveryAndInvalidBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-route-credential"), "chosen")
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{"review_finding_route": {7, 6}, "review_route_read": {1, 1}, "review_routes_list": {3, 1}, "review_route_decide": {5, 5}, "review_route_fix": {6, 6}}
	for _, tool := range listed.Tools {
		shape, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		data, _ := json.Marshal(tool.InputSchema)
		read := tool.Name == "review_route_read" || tool.Name == "review_routes_list"
		if json.Unmarshal(data, &schema) != nil || len(schema.Properties) != shape[0] || len(schema.Required) != shape[1] || schema.Additional || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != read || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint == read || !tool.Annotations.IdempotentHint {
			t.Fatal("route schema/effects widened", tool.Name)
		}
	}
	if len(want) != 0 || listed.NextCursor != "" {
		t.Fatal("route discovery incomplete", want)
	}
	id := "review_route_" + strings.Repeat("a", 32)
	evidence := "review_evidence_" + strings.Repeat("a", 32)
	valid := map[string]map[string]any{
		"review_finding_route": {"evidence_id": evidence, "idempotency_key": "key", "expected_revision": 0, "finding": 0, "target_pull_number": 4, "reason": "routing"},
		"review_route_read":    {"route_id": id}, "review_routes_list": {"pull_number": 4},
		"review_route_decide": {"route_id": id, "idempotency_key": "key", "expected_revision": 1, "disposition": "accepted", "reason": "receiving"},
		"review_route_fix":    {"route_id": id, "idempotency_key": "key", "expected_revision": 2, "expected_sha": "", "fix_sha": strings.Repeat("f", 40), "proof_note": "proof"},
	}
	for name, args := range valid {
		for field := range args {
			copy := map[string]any{}
			for k, v := range args {
				if k != field {
					copy[k] = v
				}
			}
			diagnosticResult(t, call(t, session, name, copy), "invalid_arguments", false)
			copy[field] = nil
			diagnosticResult(t, call(t, session, name, copy), "invalid_arguments", false)
		}
		for _, field := range []string{"project_id", "worker_id", "profile", "refresh"} {
			copy := map[string]any{}
			for k, v := range args {
				copy[k] = v
			}
			copy[field] = "other"
			diagnosticResult(t, call(t, session, name, copy), "invalid_arguments", false)
		}
	}
	for _, value := range []any{nil, -1, 1.5, "1"} {
		args := valid["review_finding_route"]
		copy := map[string]any{}
		for k, v := range args {
			copy[k] = v
		}
		copy["expected_route_revision"] = value
		diagnosticResult(t, call(t, session, "review_finding_route", copy), "invalid_arguments", false)
	}
	for _, args := range []map[string]any{{"pull_number": 4, "limit": nil}, {"pull_number": 4, "limit": 0}, {"pull_number": 4, "cursor": nil}, {"pull_number": 4, "cursor": ""}} {
		diagnosticResult(t, call(t, session, "review_routes_list", args), "invalid_arguments", false)
	}
	diagnosticResult(t, call(t, session, "review_finding_decide", map[string]any{"evidence_id": evidence, "idempotency_key": "key", "expected_revision": 1, "finding": 0, "disposition": "accepted", "reason": "source", "expected_route_revision": nil}), "invalid_arguments", false)
	if requests.Load() != 0 {
		t.Fatal("invalid routes loaded credentials or HTTP")
	}
}
