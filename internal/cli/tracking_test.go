package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/tracking"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

func TestTrackingCommandsActualAPI(t *testing.T) {
	f := setupServiceFixture(t)
	if err := tracking.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	tracking.Register(mux, f.store)
	f.handler = mux
	p := f.project(t, "tracking-cli")
	worker := f.worker(t, p, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}})
	invoke := func(want int, args ...string) string {
		t.Helper()
		code, out, errText := mutationInvoke(t, path, "chosen", args...)
		if code != want {
			t.Fatalf("%v: exit %d %s %s", args, code, out, errText)
		}
		return out
	}
	receipt := func(out string, revision int64, outcome string) {
		t.Helper()
		var r trackingmodel.Receipt
		if json.Unmarshal([]byte(out), &r) != nil || r.Revision != revision || r.Outcome != outcome {
			t.Fatalf("receipt %s want %d %s", out, revision, outcome)
		}
	}

	receipt(invoke(0, "milestones", "create", "v1", "--title", "First release", "--target-date", "2027-01-31"), 1, "created")
	receipt(invoke(0, "items", "create", "GR", "--title", "Grid", "--milestone", "v1"), 1, "created")
	receipt(invoke(0, "items", "create", "GR-2.1", "--title", "Room grid", "--parent", "GR", "--milestone", "v1", "--labels", "area:world,core"), 1, "created")
	receipt(invoke(0, "items", "set", "GR-2.1", "--status", "active"), 2, "updated")
	receipt(invoke(0, "items", "set", "GR-2.1", "--status", "active"), 2, "unchanged")
	receipt(invoke(0, "items", "link", "GR-2.1", "pr:3427"), 3, "updated")
	receipt(invoke(0, "items", "link", "GR-2.1", "pr:3427"), 3, "unchanged")
	// Title edits need the guard; the parser refuses them without it.
	invoke(2, "items", "set", "GR-2.1", "--title", "Renamed")
	invoke(4, "items", "set", "GR-2.1", "--expect-revision", "1", "--status", "done")
	receipt(invoke(0, "items", "set", "GR-2.1", "--expect-revision", "3", "--title", "Renamed", "--clear-parent"), 4, "updated")

	var view trackingmodel.ItemView
	if json.Unmarshal([]byte(invoke(0, "items", "read", "GR-2.1")), &view) != nil || view.Item.Title != "Renamed" || view.Item.Parent != "" || len(view.Item.Links) != 1 || view.Item.Links[0].Ref != "example/tracking-cli#3427" {
		t.Fatalf("read: %+v", view)
	}
	var page trackingmodel.Page[trackingmodel.ItemSummary]
	if json.Unmarshal([]byte(invoke(0, "items", "list", "--link", "pr:3427")), &page) != nil || len(page.Items) != 1 || page.Items[0].Key != "GR-2.1" {
		t.Fatalf("list by link: %+v", page)
	}
	var m trackingmodel.MilestoneView
	if json.Unmarshal([]byte(invoke(0, "milestones", "read", "v1")), &m) != nil || m.Counts.Total != 2 || m.Counts.Active != 1 || len(m.OpenItems) != 2 {
		t.Fatalf("milestone: %+v", m)
	}
	receipt(invoke(0, "items", "unlink", "GR-2.1", "pr:3427"), 5, "updated")
	receipt(invoke(0, "milestones", "set", "v1", "--state", "closed"), 2, "updated")

	batch := filepath.Join(t.TempDir(), "batch.json")
	doc := `{"milestones":[{"key":"v2","milestone":{"title":"Second"}}],"items":[{"key":"X","item":{"title":"X","milestone":"v2","status":"deferred","revisit_when":"after v1"}}]}`
	if err := os.WriteFile(batch, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out := invoke(0, "items", "upsert", "--file", batch)
	if !strings.Contains(out, `"outcome":"created"`) {
		t.Fatal(out)
	}
	if out = invoke(0, "items", "upsert", "--file", batch); strings.Contains(out, "created") || strings.Contains(out, "updated") {
		t.Fatalf("reapply changed records: %s", out)
	}
	var history trackingmodel.Page[trackingmodel.HistorySummary]
	if json.Unmarshal([]byte(invoke(0, "items", "history", "GR-2.1", "--limit", "2")), &history) != nil || len(history.Items) != 2 || !history.HasMore {
		t.Fatalf("history: %+v", history)
	}
	var rev trackingmodel.ItemRevision
	if json.Unmarshal([]byte(invoke(0, "items", "revision", "GR-2.1", "1")), &rev) != nil || rev.State.Status != "planned" {
		t.Fatalf("revision: %+v", rev)
	}
	invoke(2, "items", "frobnicate", "X")
	invoke(2, "items", "create", "bad key", "--title", "x")
}
