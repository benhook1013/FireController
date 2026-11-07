package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/client"
)

func clientObligationBatch() adoptionholdsmodel.Batch {
	zero := int64(0)
	return adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "sealed-source", RepositoryID: 7, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []adoptionholdsmodel.Obligation{{SourceKey: "finding-one", Kind: "finding", AffectedPullNumbers: []int64{7}, Summary: "retain correction", Evidence: []string{}, OriginalStatus: "open"}}}
}

func TestAdoptionObligationTypedReadsAndWrites(t *testing.T) {
	for _, kind := range []string{"finding", "route", "result"} {
		t.Run(kind, func(t *testing.T) { testAdoptionObligationTypedReadsAndWrites(t, kind) })
	}
}

func testAdoptionObligationTypedReadsAndWrites(t *testing.T, kind string) {
	id := "adoption_obligation_00000000000000000000000000000000"
	batch := clientObligationBatch()
	batch.Obligations[0].Kind = kind
	raw, _ := json.Marshal(batch)
	canonical, err := client.NormalizeAdoptionObligationDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instance") {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("request lost worker scope")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/adoption-obligations"):
			if r.URL.Query().Get("pull_number") != "7" || r.URL.Query().Get("status") != "open" || r.URL.Query().Get("limit") != "10" {
				t.Error("list filters changed", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Page{Items: []adoptionholdsmodel.ListItem{{ID: id, Namespace: "sealed-source", SourceKey: "finding-one", Kind: kind, Status: "open", Revision: 1, AffectedPullNumbers: []int64{7}}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+id):
			_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Detail{ID: id, ProjectID: "project-one", Source: batch.Source, SourceRevision: 1, Original: batch.Obligations[0], AffectedPullNumbers: batch.Obligations[0].AffectedPullNumbers, Status: "open", Revision: 1, ActorID: "principal-one", WorkerID: "worker-one", CreatedAt: "2026-10-20T00:00:00Z"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/preview"):
			var got adoptionholdsmodel.Batch
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.Obligations[0].SourceKey != "finding-one" || got.Obligations[0].Kind != kind || r.Header.Get("Idempotency-Key") != "" {
				t.Error("preview body changed")
			}
			_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Preview{ProjectID: "project-one", SourceRevision: 0, Selected: 1, New: 1, Unchanged: 0, PayloadSHA256: adoptionholdsmodel.Digest(canonical), PreviewSHA256: strings.Repeat("b", 64)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/apply"):
			if r.Header.Get("Idempotency-Key") != "adopt-key" {
				t.Error("apply key changed")
			}
			_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Receipt{Outcome: "imported", Revision: 1, New: 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/"+id+"/resolve"):
			if r.Header.Get("Idempotency-Key") != "resolve-key" {
				t.Error("resolve key changed")
			}
			_ = json.NewEncoder(w).Encode(client.AdoptionObligationResolutionReceipt{ID: id, Status: "resolved", Revision: 2})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	service := newClient(t, server)
	if page, err := service.AdoptionObligations(context.Background(), 7, "open", client.PageOptions{Limit: 10}); err != nil || len(page.Items) != 1 || page.Items[0].Kind != kind {
		t.Fatal("list failed", err, page)
	}
	if detail, err := service.AdoptionObligation(context.Background(), id); err != nil || detail.ID != id || detail.Resolution != nil || detail.Original.Kind != kind {
		t.Fatal("detail failed", err, detail)
	}
	preview, err := service.PreviewAdoptionObligations(context.Background(), raw)
	if err != nil {
		t.Fatal("preview failed", err)
	}
	if receipt, err := service.ApplyAdoptionObligations(context.Background(), "adopt-key", preview.PreviewSHA256, raw); err != nil || receipt.Outcome != "imported" {
		t.Fatal("apply failed", err, receipt)
	}
	one := int64(1)
	resolution := client.AdoptionObligationResolveRequest{ExpectedRevision: &one, Reason: "addressed", Evidence: []string{"native:proof"}}
	if receipt, err := service.ResolveAdoptionObligation(context.Background(), id, "resolve-key", resolution); err != nil || receipt.Revision != 2 {
		t.Fatal("resolve failed", err, receipt)
	}
	if calls.Load() != 5 {
		t.Fatal("unexpected request count", calls.Load())
	}
}

func TestAdoptionObligationValidationBeforeTransport(t *testing.T) {
	service := &client.Client{}
	one := int64(1)
	if _, err := service.ResolveAdoptionObligation(context.Background(), "adoption_obligation_00000000000000000000000000000000", "key", client.AdoptionObligationResolveRequest{ExpectedRevision: &one, Reason: "addressed", Evidence: []string{}}); err == nil {
		t.Fatal("empty resolution evidence reached transport")
	}
	if _, err := client.NormalizeAdoptionObligationDocument([]byte(`{"version":1,"unknown":true}`)); err == nil {
		t.Fatal("unknown batch field admitted")
	}
	batch := clientObligationBatch()
	batch.Obligations[0].Kind = "unknown-result"
	raw, _ := json.Marshal(batch)
	if _, err := client.NormalizeAdoptionObligationDocument(raw); err == nil {
		t.Fatal("unknown obligation kind normalized")
	}
	if _, err := service.PreviewAdoptionObligations(context.Background(), raw); err == nil {
		t.Fatal("unknown obligation kind reached preview transport")
	}
}

func TestAdoptionObligationUnknownKindResponsesRefuse(t *testing.T) {
	id := "adoption_obligation_00000000000000000000000000000000"
	batch := clientObligationBatch()
	batch.Obligations[0].Kind = "unknown-result"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instance") {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/adoption-obligations") {
			_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Page{Items: []adoptionholdsmodel.ListItem{{ID: id, Namespace: batch.Source.Namespace, SourceKey: batch.Obligations[0].SourceKey, Kind: "unknown-result", Status: "open", Revision: 1, AffectedPullNumbers: []int64{7}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(adoptionholdsmodel.Detail{ID: id, ProjectID: "project-one", Source: batch.Source, SourceRevision: 1, Original: batch.Obligations[0], AffectedPullNumbers: batch.Obligations[0].AffectedPullNumbers, Status: "open", Revision: 1, ActorID: "principal-one", WorkerID: "worker-one", CreatedAt: "2026-10-20T00:00:00Z"})
	}))
	defer server.Close()
	service := newClient(t, server)
	_, err := service.AdoptionObligations(context.Background(), 7, "open", client.PageOptions{Limit: 10})
	checkError(t, err, "invalid_response")
	_, err = service.AdoptionObligation(context.Background(), id)
	checkError(t, err, "invalid_response")
}

// Exercise the same typed detail read used by both CLI and MCP, including
// escaped content at all three admission boundaries and maximum current scope.
func TestAdoptionObligationFullDetailEncodedBudget(t *testing.T) {
	one, two := int64(1), int64(2)
	target := int64(2147483647)
	original := adoptionholdsmodel.Obligation{SourceKey: strings.Repeat("<", 200), Kind: "route", Summary: strings.Repeat("<", 4500), Evidence: []string{}, OriginalStatus: "pending"}
	for number := target - 100; number < target; number++ {
		original.AffectedPullNumbers = append(original.AffectedPullNumbers, number)
	}
	original.Summary += strings.Repeat("x", (32<<10)-len(adoptionholdsmodel.Encode(original)))
	resolution := adoptionholdsmodel.ResolveRequest{ExpectedRevision: &two, Reason: strings.Repeat("<", 2600), Evidence: []string{"proof:"}}
	resolution.Evidence[0] += strings.Repeat("x", (16<<10)-len(adoptionholdsmodel.Encode(resolution)))
	retarget := adoptionholdsmodel.RetargetRequest{ExpectedRevision: &one, TargetPullNumber: &target, Reason: strings.Repeat("<", 2600)}
	retarget.Reason += strings.Repeat("x", (16<<10)-len(adoptionholdsmodel.Encode(retarget)))
	if _, err := adoptionholdsmodel.NormalizeObligation(original); err != nil {
		t.Fatal("maximum original is not admissible", err)
	}
	if _, err := adoptionholdsmodel.NormalizeResolution(resolution); err != nil {
		t.Fatal("maximum resolution is not admissible", err)
	}
	if _, err := adoptionholdsmodel.NormalizeRetarget(retarget); err != nil {
		t.Fatal("maximum retarget is not admissible", err)
	}
	if len(adoptionholdsmodel.Encode(original)) != 32<<10 || len(adoptionholdsmodel.Encode(resolution)) != 16<<10 || len(adoptionholdsmodel.Encode(retarget)) != 16<<10 {
		t.Fatal("fixture lost an exact encoded admission boundary")
	}
	id := "adoption_obligation_00000000000000000000000000000000"
	actor := strings.Repeat("a", 128)
	at := "2026-10-20T00:00:00.123456789+00:00"
	detail := adoptionholdsmodel.Detail{
		ID: id, ProjectID: "project-one", Source: adoptionholdsmodel.Source{Namespace: strings.Repeat("<", 200), RepositoryID: 9223372036854775807, SnapshotSHA256: strings.Repeat("a", 64)},
		SourceRevision: 9223372036854775807, Original: original, Status: "resolved", Revision: 3, ActorID: actor, WorkerID: actor, CreatedAt: at,
		AffectedPullNumbers: adoptionholdsmodel.RouteAffectedPullNumbers(original, target),
		Retarget:            &adoptionholdsmodel.Retarget{Revision: 2, TargetPullNumber: target, Reason: retarget.Reason, ActorID: actor, WorkerID: actor, ChangedAt: at},
		Resolution:          &adoptionholdsmodel.Resolution{Revision: 3, Reason: resolution.Reason, Evidence: resolution.Evidence, ActorID: actor, WorkerID: actor, ResolvedAt: at},
	}
	body := append(adoptionholdsmodel.Encode(detail), '\n')
	if len(detail.AffectedPullNumbers) != 101 || len(body) <= 64<<10 || len(body) >= 80<<10 {
		t.Fatal("fixture must exceed the old cap but fit the proved bound", len(body))
	}
	// Conservative proof independent of the fixture's shorter generated IDs and
	// timestamps: all bounded strings may expand sixfold under JSON escaping.
	const metadataBound = (8*128+200+64+3*64)*6 + 101*11 + 2 + 10*20 + (2 << 10)
	if metadataBound >= 16<<10 || ((32+16+16)<<10)+metadataBound >= 80<<10 {
		t.Fatal("detail metadata no longer fits its documented conservative bound")
	}
	t.Logf("original=%d resolution_request=%d retarget_request=%d detail=%d conservative_metadata=%d", len(adoptionholdsmodel.Encode(original)), len(adoptionholdsmodel.Encode(resolution)), len(adoptionholdsmodel.Encode(retarget)), len(body), metadataBound)
	var response atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instance") {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response.Load().([]byte))
	}))
	defer server.Close()
	service := newClient(t, server)
	for _, size := range []int{len(body), 96 << 10, (96 << 10) + 1} {
		response.Store(append(append([]byte(nil), body...), bytes.Repeat([]byte(" "), size-len(body))...))
		got, err := service.AdoptionObligation(context.Background(), id)
		if size > 96<<10 {
			checkError(t, err, "invalid_response")
		} else if err != nil || !reflect.DeepEqual(got, detail) {
			t.Fatalf("full detail at %d bytes was not preserved: %v", size, err)
		}
	}
}
