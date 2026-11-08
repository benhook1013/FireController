package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type adoptionWireCase struct {
	name, method, path string
	args               map[string]any
	write              bool
}

func adoptionWireCases(t *testing.T) []adoptionWireCase {
	t.Helper()
	zero, one := int64(0), int64(1)
	hash := strings.Repeat("a", 64)
	obligation := "adoption_obligation_" + strings.Repeat("d", 32)
	raw := func(value any) json.RawMessage {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	rounds := raw(client.PreparedReviewRoundControl{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "prepared archive", SourceKey: "2:cli", SourceSHA256: hash, MinimumRemaining: 0, MaximumRemaining: 2, InitialDry: 1, Taper: 2})
	obligations := raw(adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "prepared", RepositoryID: 1, SnapshotSHA256: hash}, ExpectedRevision: &zero, Obligations: []adoptionholdsmodel.Obligation{{SourceKey: "one", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "Retained obligation", Evidence: []string{}, OriginalStatus: "open"}}})
	preview := func(doc json.RawMessage) map[string]any { return map[string]any{"document": doc} }
	apply := func(doc json.RawMessage) map[string]any {
		return map[string]any{"document": doc, "preview_sha256": hash, "idempotency_key": "chosen"}
	}
	return []adoptionWireCase{
		{"review_round_adoption_preview", "POST", "reviews/rounds/adoption/preview", preview(rounds), false}, {"review_round_adoption_apply", "POST", "reviews/rounds/adoption/apply", apply(rounds), true},
		{"review_obligations_list", "GET", "reviews/adoption-obligations", map[string]any{}, false}, {"review_obligation_read", "GET", "reviews/adoption-obligations/" + obligation, map[string]any{"obligation_id": obligation}, false},
		{"review_obligations_import_preview", "POST", "reviews/adoption-obligations/preview", preview(obligations), false}, {"review_obligations_import_apply", "POST", "reviews/adoption-obligations/apply", apply(obligations), true},
		{"review_obligation_retarget", "POST", "reviews/adoption-obligations/" + obligation + "/retarget", map[string]any{"obligation_id": obligation, "expected_revision": 1, "target_pull_number": 0, "reason": "Receiving work remains open", "idempotency_key": "chosen"}, true},
		{"review_obligation_resolve", "POST", "reviews/adoption-obligations/" + obligation + "/resolve", map[string]any{"obligation_id": obligation, "document": client.AdoptionObligationResolveRequest{ExpectedRevision: &one, Reason: "Evidence verified", Evidence: []string{"proof:one"}}, "idempotency_key": "chosen"}, true},
	}
}
func TestAdoptionOfficialSDKBoundTransportAndPermissionRefusal(t *testing.T) {
	var count atomic.Int64
	var active atomic.Value
	active.Store(adoptionWireCase{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			json.NewEncoder(w).Encode(map[string]string{"instance_id": "instance-one"})
			return
		}
		count.Add(1)
		current := active.Load().(adoptionWireCase)
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
	for _, entry := range adoptionWireCases(t) {
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

func TestAdoptionMCPInvalidArgumentsBeforeHTTP(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); t.Error("invalid request reached HTTP") }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	for _, entry := range adoptionWireCases(t) {
		t.Run(entry.name, func(t *testing.T) {
			for _, field := range []string{"project_id", "worker_id", "server_url", "unexpected"} {
				bad := map[string]any{}
				for key, value := range entry.args {
					bad[key] = value
				}
				bad[field] = "other"
				diagnosticResult(t, call(t, session, entry.name, bad), "invalid_arguments", false)
			}
			for key := range entry.args {
				bad := map[string]any{}
				for k, value := range entry.args {
					bad[k] = value
				}
				bad[key] = nil
				diagnosticResult(t, call(t, session, entry.name, bad), "invalid_arguments", false)
			}
		})
	}
	for _, bad := range []map[string]any{{"status": ""}, {"pull_number": 0}, {"limit": 0}, {"cursor": ""}} {
		diagnosticResult(t, call(t, session, "review_obligations_list", bad), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input contacted service")
	}
}
