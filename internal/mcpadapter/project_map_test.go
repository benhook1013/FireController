package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mapSDKFixture(t *testing.T) *mutationAPI {
	t.Helper()
	f := mutationFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	f.handler = mux
	return f
}
func mapSDKCounts(t *testing.T, f *mutationAPI) []int {
	t.Helper()
	var counts []int
	for _, table := range []string{"events", "receipts"} {
		var n int
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, n)
	}
	return counts
}
func TestProjectMapToolsSDKActualAPIReplayPaginationAndReferences(t *testing.T) {
	f := mapSDKFixture(t)
	var lose atomic.Bool
	lose.Store(true)
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/map/entries") || strings.Contains(r.URL.Path, "/workstream") {
			if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.sender {
				t.Error("map tool lost bound identity")
			}
			if r.Method != "GET" {
				writes.Add(1)
				if r.Header.Get("Idempotency-Key") == "" {
					t.Error("map mutation lost explicit key")
				}
			}
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/map/entries") && lose.CompareAndSwap(true, false) {
			record := httptest.NewRecorder()
			f.handler.ServeHTTP(record, r)
			if record.Code != 201 {
				t.Errorf("map did not commit %d", record.Code)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(record.Code)
			_, _ = io.WriteString(w, "{")
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": identity}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"map_entry_create": true, "map_entry_replace": true, "map_entry_list": true, "map_entry_read": true, "map_entry_history": true, "map_entry_revision_read": true, "job_workstream_read": true, "job_workstream_replace": true, "job_workstream_history": true}
	for _, tool := range tools.Tools {
		if !wanted[tool.Name] {
			continue
		}
		delete(wanted, tool.Name)
		write := tool.Name == "map_entry_create" || tool.Name == "map_entry_replace" || tool.Name == "job_workstream_replace"
		destructive := tool.Name == "map_entry_replace" || tool.Name == "job_workstream_replace"
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint != destructive || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatal("map tool effects", tool.Name)
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(raw), "profile") || strings.Contains(string(raw), "project_id") {
			t.Fatal("map tool exposed binding selector")
		}
	}
	if len(wanted) != 0 {
		t.Fatal("map tools missing across discovery pages", wanted)
	}
	entry := map[string]any{"kind": "workstream", "title": " Area ", "description": "PRIVATE_MAP <script>literal</script>", "links": nil, "phases": []any{map[string]any{"id": "first", "name": " First "}}}
	request := map[string]any{"idempotency_key": "sdk-create-once", "entry": entry}
	diagnosticResult(t, call(t, session, "map_entry_create", request), "invalid_response", true)
	if writes.Load() != 1 {
		t.Fatal("lost response was retried")
	}
	before := mapSDKCounts(t, f)
	// Recover the committed SDK action through the ordinary CLI with exactly the same key/content/binding.
	file := filepath.Join(t.TempDir(), "entry.json")
	data, _ := json.Marshal(entry)
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "map", "create", "--file", file, "--key", "sdk-create-once"}, nil, &stdout, &stderr)
	var original client.ProjectMapReceipt
	if code != 0 || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &original) != nil || !reflect.DeepEqual(before, mapSDKCounts(t, f)) || strings.Contains(stdout.String(), "PRIVATE_MAP") {
		t.Fatal("cross-adapter original-key recovery", code, stdout.String(), stderr.String())
	}
	var current client.ProjectMapEntry
	decodeResult(t, call(t, session, "map_entry_read", map[string]any{"entry_id": original.ID}), &current)
	if current.Title != "Area" || current.ProjectID != f.project || current.Description != entry["description"] || current.Phases[0].Name != "First" {
		t.Fatal(current)
	}
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "sdk-map-job", "title": "Job", "brief": "UNCHANGED_BRIEF"}), &job)
	var assoc client.JobWorkstream
	decodeResult(t, call(t, session, "job_workstream_read", map[string]any{"job_id": job.ID}), &assoc)
	if assoc.Revision != 0 || assoc.WorkstreamID != "" {
		t.Fatal(assoc)
	}
	stale := diagnosticResult(t, call(t, session, "job_workstream_replace", map[string]any{"job_id": job.ID, "idempotency_key": "sdk-stale", "expected_revision": 1, "workstream_id": original.ID}), "job_workstream_revision_conflict", false)
	if details, ok := stale.Details.(map[string]any); !ok || details["current_revision"] != float64(0) {
		t.Fatal(stale)
	}
	set := map[string]any{"job_id": job.ID, "idempotency_key": "sdk-set", "expected_revision": 0, "workstream_id": original.ID}
	var associated client.JobWorkstreamReceipt
	decodeResult(t, call(t, session, "job_workstream_replace", set), &associated)
	if associated.Revision != 1 || associated.JobID != job.ID {
		t.Fatal(associated)
	}
	entry["title"] = "Renamed"
	entry["phases"].([]any)[0].(map[string]any)["name"] = "Renamed phase"
	replace := map[string]any{"idempotency_key": "sdk-rename", "entry_id": original.ID, "expected_revision": 1, "entry": entry}
	var updated client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_replace", replace), &updated)
	if updated.Revision != 2 || updated.Outcome != "updated" {
		t.Fatal(updated)
	}
	decodeResult(t, call(t, session, "job_workstream_read", map[string]any{"job_id": job.ID}), &assoc)
	if assoc.WorkstreamTitle != "Area" || assoc.Revision != 1 {
		t.Fatal("current association lost captured meaning", assoc)
	}
	var refHistory client.Page[client.JobWorkstreamHistoryEntry]
	decodeResult(t, call(t, session, "job_workstream_history", map[string]any{"job_id": job.ID, "limit": 1}), &refHistory)
	if len(refHistory.Items) != 1 || refHistory.Items[0].WorkstreamTitle != "Area" || refHistory.Items[0].JobRevision != 1 {
		t.Fatal(refHistory)
	}
	var another client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_create", map[string]any{"idempotency_key": "sdk-return", "entry": map[string]any{"kind": "return_point", "title": "Later", "trigger": "when ready"}}), &another)
	var page client.Page[client.ProjectMapSummary]
	listed := call(t, session, "map_entry_list", map[string]any{"limit": 1})
	decodeResult(t, listed, &page)
	encoded, _ := json.Marshal(listed.StructuredContent)
	if len(page.Items) != 1 || !page.HasMore || strings.Contains(string(encoded), "PRIVATE_MAP") || strings.Contains(string(encoded), "description") {
		t.Fatal("noncompact list", string(encoded))
	}
	decodeResult(t, call(t, session, "map_entry_list", map[string]any{"limit": 1, "cursor": page.Cursor}), &page)
	if len(page.Items) != 1 || page.HasMore {
		t.Fatal(page)
	}
	decodeResult(t, call(t, session, "map_entry_list", map[string]any{"kind": "return_point", "archived": false}), &page)
	if len(page.Items) != 1 || page.Items[0].ID != another.ID {
		t.Fatal(page)
	}
	var history client.Page[client.ProjectMapHistorySummary]
	decodeResult(t, call(t, session, "map_entry_history", map[string]any{"entry_id": original.ID, "limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 2 || !history.HasMore {
		t.Fatal(history)
	}
	decodeResult(t, call(t, session, "map_entry_history", map[string]any{"entry_id": original.ID, "limit": 1, "cursor": history.Cursor}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 1 || history.HasMore {
		t.Fatal(history)
	}
	var old client.ProjectMapRevision
	decodeResult(t, call(t, session, "map_entry_revision_read", map[string]any{"entry_id": original.ID, "revision": 1}), &old)
	if old.State.Title != "Area" || old.State.Phases[0].Name != "First" || old.ActorID == "" {
		t.Fatal(old)
	}
	var clear client.JobWorkstreamReceipt
	decodeResult(t, call(t, session, "job_workstream_replace", map[string]any{"job_id": job.ID, "idempotency_key": "sdk-clear", "expected_revision": 1, "workstream_id": ""}), &clear)
	before = mapSDKCounts(t, f)
	var replay client.JobWorkstreamReceipt
	decodeResult(t, call(t, session, "job_workstream_replace", set), &replay)
	if replay != associated {
		t.Fatal("association replay changed after clear", replay)
	}
	decodeResult(t, call(t, session, "map_entry_replace", replace), &another)
	if another != updated {
		t.Fatal("replacement replay changed", another)
	}
	decodeResult(t, call(t, session, "job_workstream_read", map[string]any{"job_id": job.ID}), &assoc)
	if assoc.Revision != 2 || assoc.WorkstreamID != "" || !reflect.DeepEqual(before, mapSDKCounts(t, f)) || f.providers.Load() != 1 {
		t.Fatal("reads/replay wrote, resurrected reference or used provider", assoc)
	}
}
func TestProjectMapToolsInvalidBeforeCredentialOrHTTP(t *testing.T) {
	var hits atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": identity}, "missing-private-token"), "chosen"), "")
	for _, test := range []struct{ name, raw string }{
		{"map_entry_create", `{"idempotency_key":"k","entry":null}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"x","Title":"alias"}}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"x","archived":null}}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"x","phases":[{"id":"p","name":"n","state":null}]}}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"\ud800"}}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"x","links":[{"label":"x","url":"javascript:bad"}]}}`}, {"map_entry_create", `{"idempotency_key":"k","entry":{"kind":"workstream","title":"x","title":"y"}}`},
		{"map_entry_replace", `{"idempotency_key":"k","entry_id":"id","expected_revision":0,"entry":{"kind":"workstream","title":"x"}}`}, {"map_entry_replace", `{"idempotency_key":"k","entry_id":"id","expected_revision":9223372036854775807,"entry":{"kind":"workstream","title":"x"}}`},
		{"map_entry_list", `{"limit":0}`}, {"map_entry_list", `{"kind":"phase"}`}, {"map_entry_list", `{"archived":"false"}`}, {"map_entry_list", `{"archived":null}`}, {"map_entry_list", `{"cursor":" padded"}`}, {"map_entry_read", `{"entry_id":"../foreign"}`}, {"map_entry_history", `{"entry_id":"id","worker_id":"other"}`}, {"map_entry_revision_read", `{"entry_id":"id","revision":0}`},
		{"job_workstream_read", `{"job_id":"id","project_id":"other"}`}, {"job_workstream_replace", `{"job_id":"id","idempotency_key":"k","expected_revision":0}`}, {"job_workstream_replace", `{"job_id":"id","idempotency_key":"k","expected_revision":null,"workstream_id":""}`}, {"job_workstream_replace", `{"job_id":"id","idempotency_key":"k","expected_revision":9223372036854775807,"workstream_id":""}`}, {"job_workstream_replace", `{"job_id":"id","idempotency_key":"k","expected_revision":0,"workstream_id":null}`}, {"job_workstream_history", `{"job_id":"id","cursor":""}`},
	} {
		diagnosticResult(t, call(t, session, test.name, json.RawMessage(test.raw)), "invalid_arguments", false)
	}
	if hits.Load() != 0 {
		t.Fatal("invalid map tool used credentials/network", hits.Load())
	}
}
func TestProjectMapToolsCancellationDoesNotWriteOrBlockReuse(t *testing.T) {
	f := mapSDKFixture(t)
	entered := make(chan struct{})
	var block atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && strings.HasSuffix(r.URL.Path, "/map/entries") {
			close(entered)
			<-r.Context().Done()
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	before := mapSDKCounts(t, f)
	block.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() {
		_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "map_entry_list", Arguments: map[string]any{}})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("map SDK read did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDK cancellation blocked")
	}
	if err == nil || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("cancelled read returned success or changed ledger", err)
	}
	block.Store(false)
	var page client.Page[client.ProjectMapSummary]
	decodeResult(t, call(t, session, "map_entry_list", map[string]any{}), &page)
	if page.Items == nil || len(page.Items) != 0 || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("SDK reuse changed state", page)
	}
}

func TestProjectMapToolsSectionPositionAndDiscovery(t *testing.T) {
	f := mapSDKFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": identity}, f.credential), "chosen"), "")
	entry := map[string]any{"kind": "section", "title": "Narrative", "description": "# Markdown <literal>", "position": 42}
	var receipt client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_create", map[string]any{"idempotency_key": "section-sdk", "entry": entry}), &receipt)
	var current client.ProjectMapEntry
	decodeResult(t, call(t, session, "map_entry_read", map[string]any{"entry_id": receipt.ID}), &current)
	if current.Kind != "section" || current.Position != 42 || current.Description != entry["description"] {
		t.Fatal(current)
	}
	var page client.Page[client.ProjectMapSummary]
	decodeResult(t, call(t, session, "map_entry_list", map[string]any{"kind": "section"}), &page)
	if len(page.Items) != 1 || page.Items[0].ID != receipt.ID {
		t.Fatal(page)
	}
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, tool := range tools.Tools {
		if tool.Name == "map_entry_create" || tool.Name == "map_entry_replace" || tool.Name == "map_entry_list" {
			raw, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(raw), `"section"`) {
				t.Fatal("section missing from schema", tool.Name)
			}
			if tool.Name != "map_entry_list" && !strings.Contains(string(raw), `"position"`) {
				t.Fatal("position missing from schema", tool.Name)
			}
			found++
		}
	}
	if found != 3 {
		t.Fatal(found)
	}
	before := mapSDKCounts(t, f)
	for _, raw := range []string{`{"idempotency_key":"bad","entry":{"kind":"section","title":"x","position":null}}`, `{"idempotency_key":"bad","entry":{"kind":"section","title":"x","position":1000001}}`, `{"idempotency_key":"bad","entry":{"kind":"section","title":"x","trigger":"wrong"}}`} {
		diagnosticResult(t, call(t, session, "map_entry_create", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("invalid tool invocation mutated state")
	}
}
