package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/tracking"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

func TestTrackingToolsSDKActualAPI(t *testing.T) {
	f := mutationFixture(t)
	if err := tracking.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	tracking.Register(mux, f.store)
	service := httptest.NewServer(mux)
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": identity}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")

	var r trackingmodel.Receipt
	decodeResult(t, call(t, session, "milestone_upsert", map[string]any{"key": "v1", "milestone": map[string]any{"title": "First"}}), &r)
	if r.Outcome != "created" {
		t.Fatal(r)
	}
	decodeResult(t, call(t, session, "item_upsert", map[string]any{"key": "A", "item": map[string]any{"title": "A", "milestone": "v1"}}), &r)
	decodeResult(t, call(t, session, "item_update", map[string]any{"key": "A", "changes": map[string]any{"status": "active", "add_labels": []string{"core"}}}), &r)
	if r.Revision != 2 || r.Outcome != "updated" {
		t.Fatal(r)
	}
	decodeResult(t, call(t, session, "item_links_change", map[string]any{"key": "A", "add": []map[string]string{{"type": "pr", "ref": "7", "role": "delivers"}}}), &r)
	if r.Revision != 3 {
		t.Fatal(r)
	}
	var view trackingmodel.ItemView
	decodeResult(t, call(t, session, "item_read", map[string]any{"key": "A"}), &view)
	if view.Item.Status != "active" || len(view.Item.Links) != 1 || view.Item.Labels[0] != "core" {
		t.Fatal(view)
	}
	var page trackingmodel.Page[trackingmodel.ItemSummary]
	decodeResult(t, call(t, session, "item_list", map[string]any{"milestone_key": "v1", "status": []string{"active"}}), &page)
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	var m trackingmodel.MilestoneView
	decodeResult(t, call(t, session, "milestone_read", map[string]any{"key": "v1"}), &m)
	if m.Counts.Active != 1 || len(m.OpenItems) != 1 {
		t.Fatal(m)
	}
	var batch trackingmodel.BatchReceipt
	decodeResult(t, call(t, session, "tracking_batch", map[string]any{"document": map[string]any{"items": []map[string]any{{"key": "B", "item": map[string]any{"title": "B", "parent": "A"}}}}}), &batch)
	if len(batch.Results) != 1 || batch.Results[0].Outcome != "created" {
		t.Fatal(batch)
	}
	var history trackingmodel.Page[trackingmodel.HistorySummary]
	decodeResult(t, call(t, session, "tracking_history", map[string]any{"kind": "items", "key": "A"}), &history)
	if len(history.Items) != 3 {
		t.Fatal(history)
	}
	// Title edits need the guard; null and unknown fields are refused locally.
	diagnosticResult(t, call(t, session, "item_update", map[string]any{"key": "A", "changes": map[string]any{"title": "x"}}), "validation_failed", false)
	diagnosticResult(t, call(t, session, "item_update", map[string]any{"key": "A", "changes": map[string]any{"status": nil}}), "invalid_arguments", false)
	diagnosticResult(t, call(t, session, "item_read", map[string]any{"key": "A", "kind": "items"}), "invalid_arguments", false)
}
