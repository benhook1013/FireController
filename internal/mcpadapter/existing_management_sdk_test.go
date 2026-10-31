package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
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
	t.Helper()
	zero := int64(0)
	one := int64(1)
	hash := strings.Repeat("a", 64)
	id := "history_collection_" + strings.Repeat("b", 32)
	page := "history_page_" + strings.Repeat("c", 32)
	obligation := "adoption_obligation_" + strings.Repeat("d", 32)
	raw := func(v any) json.RawMessage {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	pub := json.RawMessage(`{"expected_revision":0,"enabled":true,"manifest":{"slug":"example","presentation":{"name":"Approved public project"},"description":"Approved content"}}`)
	coord := raw(client.CoordinationImportBatch{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, Source: "prepared archive", SourceSHA256: hash, Jobs: []coordimportmodel.PreparedJob{{SourceKey: "job-one", Title: "Retained job", Brief: "Private working instructions", SourceRevision: 2, Status: "parked", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", Checkpoints: []coordimportmodel.PreparedCheckpoint{}}}})
	imported := json.RawMessage(`{"expected_revision":0,"repository_id":1,"source":"prepared archive","observed_at":"2026-01-01T00:00:00Z","queue":[],"retired":[]}`)
	transferred := raw(reviews.TransferRequest{Version: 1, ExpectedRevision: &zero, RepositoryID: 1, Publisher: "prepared archive", Groups: []reviews.TransferGroup{{Source: "original", Pulls: []reviews.PullInput{{Number: 2, Title: "Retained PR", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{}, Attempts: []reviews.Attempt{}}}}}})
	rounds := raw(client.PreparedReviewRoundControl{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "prepared archive", SourceKey: "2:cli", SourceSHA256: hash, MinimumRemaining: 0, MaximumRemaining: 2, InitialDry: 1, Taper: 2})
	obligations := raw(adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "prepared", RepositoryID: 1, SnapshotSHA256: hash}, ExpectedRevision: &zero, Obligations: []adoptionholdsmodel.Obligation{{SourceKey: "one", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "Retained obligation", Evidence: []string{}, OriginalStatus: "open"}}})
	mapped := raw(client.MapImportDocument{Version: 1, Source: client.MapImportSource{Key: "prepared-map", RepositoryID: 1, Kind: "prepared", Snapshot: "selected docs", CollectedAt: "2026-01-01T00:00:00Z", TransformationVersion: 1, Scope: "selected sources"}, Entries: []client.MapImportItem{{SourceKey: "one", Entry: client.ProjectMapEditable{Kind: "workstream", Title: "Selected work", Description: "Retained description", State: "planned", Phases: []client.ProjectMapPhase{}, Links: []client.ProjectMapLink{}}, Provenance: client.MapImportProvenance{References: []string{"https://example.test/source"}, Inferred: true, Confidence: "medium", Uncertainty: "Proposed grouping"}}}})
	preview := func(doc json.RawMessage) map[string]any { return map[string]any{"document": doc} }
	apply := func(doc json.RawMessage) map[string]any {
		return map[string]any{"document": doc, "preview_sha256": hash, "idempotency_key": "chosen"}
	}
	change := func() map[string]any {
		return map[string]any{"collection_id": id, "expected_revision": 1, "idempotency_key": "chosen"}
	}
	return []managementWireCase{
		{"presentation_replace", "PUT", "presentation", map[string]any{"document": raw(client.ReplacePresentationRequest{ExpectedRevision: 0, Presentation: client.PresentationConfig{Name: "Selected project"}}), "idempotency_key": "chosen"}, true},
		{"presentation_history", "GET", "presentation/history", map[string]any{}, false}, {"presentation_revision_read", "GET", "presentation/history/1", map[string]any{"revision": 1}, false},
		{"publication_configuration_read", "GET", "publication", map[string]any{}, false}, {"publication_prepare", "GET", "publication", map[string]any{"changes": map[string]any{"expected_revision": 1, "description": "Approved content"}}, false},
		{"publication_preview", "POST", "publication/preview", preview(pub), false}, {"publication_apply", "PUT", "publication", apply(pub), true}, {"publication_unpublish", "POST", "publication/unpublish", map[string]any{"expected_revision": 0, "idempotency_key": "chosen"}, true},
		{"coordination_import_read", "GET", "coordination/imports", map[string]any{}, false}, {"coordination_import_preview", "POST", "coordination/imports/preview", preview(coord), false}, {"coordination_import_apply", "POST", "coordination/imports/apply", apply(coord), true},
		{"review_import_preview", "POST", "reviews/import/preview", preview(imported), false}, {"review_import_apply", "POST", "reviews/import/apply", apply(imported), true},
		{"review_transfer_preview", "POST", "reviews/transfer/preview", preview(transferred), false}, {"review_transfer_apply", "POST", "reviews/transfer/apply", apply(transferred), true},
		{"review_round_adoption_preview", "POST", "reviews/rounds/adoption/preview", preview(rounds), false}, {"review_round_adoption_apply", "POST", "reviews/rounds/adoption/apply", apply(rounds), true},
		{"review_obligations_list", "GET", "reviews/adoption-obligations", map[string]any{}, false}, {"review_obligation_read", "GET", "reviews/adoption-obligations/" + obligation, map[string]any{"obligation_id": obligation}, false},
		{"review_obligations_import_preview", "POST", "reviews/adoption-obligations/preview", preview(obligations), false}, {"review_obligations_import_apply", "POST", "reviews/adoption-obligations/apply", apply(obligations), true},
		{"review_obligation_resolve", "POST", "reviews/adoption-obligations/" + obligation + "/resolve", map[string]any{"obligation_id": obligation, "document": client.AdoptionObligationResolveRequest{ExpectedRevision: &one, Reason: "Evidence verified", Evidence: []string{"proof:one"}}, "idempotency_key": "chosen"}, true},
		{"history_collection_enqueue", "POST", "history/collections", map[string]any{"pull_numbers": []int{2}, "idempotency_key": "chosen"}, true}, {"history_collection_read", "GET", "history/collections/" + id, map[string]any{"collection_id": id}, false},
		{"history_collection_resume", "POST", "history/collections/" + id + "/resume", change(), true}, {"history_collection_cancel", "POST", "history/collections/" + id + "/cancel", change(), true},
		{"history_collection_pages", "GET", "history/collections/" + id + "/pages", map[string]any{"collection_id": id}, false}, {"history_collection_capture_read", "GET", "history/collections/" + id + "/pages/" + page, map[string]any{"collection_id": id, "page_id": page}, false},
		{"map_import_preview", "POST", "map/import/preview", preview(mapped), false}, {"map_import_apply", "POST", "map/import", apply(mapped), true}, {"map_import_read", "GET", "map/imports/prepared-map", map[string]any{"source_key": "prepared-map"}, false},
		{"map_import_history", "GET", "map/imports/prepared-map/history", map[string]any{"source_key": "prepared-map"}, false}, {"map_import_revision_read", "GET", "map/imports/prepared-map/history/1", map[string]any{"source_key": "prepared-map", "revision": 1}, false},
		{"review_queue_edit", "POST", "reviews/queue/edits", map[string]any{"document": map[string]any{"expected_revision": 0, "repository_id": 1, "source": "superseded cleanup", "retired": []any{map[string]any{"number": 2, "reason": "superseded"}}}, "idempotency_key": "chosen"}, true},
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
