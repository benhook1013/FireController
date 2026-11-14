package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type managementWireCase struct {
	name, method, path string
	args               map[string]any
	write              bool
}

func managementWireCases(t *testing.T) []managementWireCase {
	zero := int64(0)
	hash := strings.Repeat("a", 64)
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pub := json.RawMessage(`{"expected_revision":0,"enabled":true,"manifest":{"slug":"example","presentation":{"name":"Approved public project"},"description":"Approved content"}}`)
	imported := json.RawMessage(`{"expected_revision":0,"repository_id":1,"source":"prepared archive","observed_at":"2026-01-01T00:00:00Z","queue":[],"retired":[]}`)
	transferred := raw(reviews.TransferRequest{Version: 1, ExpectedRevision: &zero, RepositoryID: 1, Publisher: "prepared archive", Groups: []reviews.TransferGroup{{Source: "original", Pulls: []reviews.PullInput{{Number: 2, Title: "Retained PR", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{}, Attempts: []reviews.Attempt{}}}}}})
	preview := func(doc json.RawMessage) map[string]any { return map[string]any{"document": doc} }
	apply := func(doc json.RawMessage) map[string]any {
		return map[string]any{"document": doc, "preview_sha256": hash, "idempotency_key": "chosen"}
	}
	return []managementWireCase{
		{"presentation_replace", "PUT", "presentation", map[string]any{"document": raw(map[string]any{"expected_revision": 0, "presentation": map[string]string{"name": "Selected project"}}), "idempotency_key": "chosen"}, true},
		{"presentation_history", "GET", "presentation/history", map[string]any{}, false}, {"presentation_revision_read", "GET", "presentation/history/1", map[string]any{"revision": 1}, false},
		{"publication_configuration_read", "GET", "publication", map[string]any{}, false},
		{"publication_preview", "POST", "publication/preview", preview(pub), false}, {"publication_apply", "PUT", "publication", apply(pub), true}, {"publication_unpublish", "POST", "publication/unpublish", map[string]any{"expected_revision": 0, "idempotency_key": "chosen"}, true},
		{"review_import_preview", "POST", "reviews/import/preview", preview(imported), false}, {"review_import_apply", "POST", "reviews/import/apply", apply(imported), true},
		{"review_transfer_preview", "POST", "reviews/transfer/preview", preview(transferred), false}, {"review_transfer_apply", "POST", "reviews/transfer/apply", apply(transferred), true},
	}
}
func TestExistingManagementOfficialSDKBoundTransportAndPermissionRefusal(t *testing.T) {
	var count atomic.Int64
	var active atomic.Value
	active.Store(managementWireCase{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			json.NewEncoder(w).Encode(map[string]string{"instance_id": "instance-one"})
			return
		}
		count.Add(1)
		current := active.Load().(managementWireCase)
		if r.Method != current.method || r.URL.Path != "/api/v1/projects/project-one/"+current.path || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("management request lost selected route or binding", r.Method, r.URL.Path, current.name)
		}
		if current.write && r.Header.Get("Idempotency-Key") != "chosen" || !current.write && r.Header.Get("Idempotency-Key") != "" {
			t.Error("mutation key changed", current.name)
		}
		core.WriteError(w, core.Fail("forbidden", "Project management is required.", 403))
	}))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": c}, credential(t)), "alpha")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, entry := range managementWireCases(t) {
		t.Run(entry.name, func(t *testing.T) {
			if !names[entry.name] {
				t.Fatal("management tool missing from real discovery")
			}
			active.Store(entry)
			before := count.Load()
			diagnosticResult(t, call(t, session, entry.name, entry.args), "forbidden", false)
			if count.Load() != before+1 {
				t.Fatal("management call failed locally or retried/fanned out", count.Load()-before)
			}
		})
	}
}

func TestPresentationReplacementRevisionGuardStaysLocal(t *testing.T) {
	var calls, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			json.NewEncoder(w).Encode(map[string]string{"instance_id": "instance-one"})
			return
		}
		writes.Add(1)
		if r.Method != "PUT" || r.URL.Path != "/api/v1/projects/project-one/presentation" || r.Header.Get("Idempotency-Key") != "chosen" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("presentation request identity changed")
		}
		var body client.ReplacePresentationRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExpectedRevision != 0 || body.Presentation.Name != "Selected" {
			t.Error("explicit zero revision was not preserved")
		}
		core.WriteError(w, core.Fail("forbidden", "Project management is required.", http.StatusForbidden))
	}))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": binding}, credential(t)), "alpha")
	for name, doc := range map[string]map[string]any{
		"missing": {"presentation": map[string]any{"name": "Selected"}},
		"null":    {"expected_revision": nil, "presentation": map[string]any{"name": "Selected"}},
		"unknown": {"expected_revision": 0, "unknown": true, "presentation": map[string]any{"name": "Selected"}},
	} {
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			diagnosticResult(t, call(t, session, "presentation_replace", map[string]any{"document": doc, "idempotency_key": "chosen"}), "invalid_arguments", false)
			if calls.Load() != before {
				t.Fatal("invalid document performed service work", calls.Load()-before)
			}
		})
	}
	before := writes.Load()
	diagnosticResult(t, call(t, session, "presentation_replace", map[string]any{"document": map[string]any{"expected_revision": 0, "presentation": map[string]string{"name": "Selected"}}, "idempotency_key": "chosen"}), "forbidden", false)
	if writes.Load() != before+1 {
		t.Fatal("explicit zero was rejected or automatically retried", writes.Load()-before)
	}
}
