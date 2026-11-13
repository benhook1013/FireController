package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestProjectMapPartialToolSDKActualAPINoHiddenReadAndReplay(t *testing.T) {
	f := mapSDKFixture(t)
	var reads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/map/entries/") {
			if r.Method == "GET" {
				reads.Add(1)
			}
			if r.Method == "PATCH" {
				writes.Add(1)
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": identity}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")
	catalog, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range catalog.Tools {
		if tool.Name == "map_entry_update" {
			found = true
			a := tool.Annotations
			if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Fatal("wrong partial mutation effects")
			}
		}
	}
	if !found {
		t.Fatal("partial update missing from discovery")
	}
	var created client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_create", map[string]any{"idempotency_key": "partial-create", "entry": map[string]any{"kind": "workstream", "title": "Area", "description": "PRIVATE_UNCHANGED", "phases": []any{map[string]any{"id": "first", "name": "First"}}}}), &created)
	request := map[string]any{"entry_id": created.ID, "expected_revision": 1, "idempotency_key": "partial-update", "changes": map[string]any{"state": "Ready"}}
	var updated client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_update", request), &updated)
	if updated.Revision != 2 || updated.Outcome != "updated" || reads.Load() != 0 || writes.Load() != 1 {
		t.Fatal(updated, reads.Load(), writes.Load())
	}
	before := mapSDKCounts(t, f)
	var replay client.ProjectMapReceipt
	decodeResult(t, call(t, session, "map_entry_update", request), &replay)
	if replay != updated || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("replay changed effects")
	}
	stale := map[string]any{"entry_id": created.ID, "expected_revision": 1, "idempotency_key": "stale", "changes": map[string]any{"state": "Ready"}}
	diagnosticResult(t, call(t, session, "map_entry_update", stale), "revision_conflict", false)
	beforeWrites := writes.Load()
	for i, changes := range []any{nil, map[string]any{}, map[string]any{"kind": "section"}, map[string]any{"state": nil}, map[string]any{"links": nil}} {
		bad := map[string]any{"entry_id": created.ID, "expected_revision": 2, "idempotency_key": string(rune('a' + i)), "changes": changes}
		diagnosticResult(t, call(t, session, "map_entry_update", bad), "invalid_arguments", false)
	}
	if writes.Load() != beforeWrites || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("invalid changes dispatched")
	}
	var current client.ProjectMapEntry
	decodeResult(t, call(t, session, "map_entry_read", map[string]any{"entry_id": created.ID}), &current)
	if current.Description != "PRIVATE_UNCHANGED" || len(current.Phases) != 1 || current.State != "Ready" {
		t.Fatal(current)
	}
	// A selected empty string clears only that field; unselected phase state survives.
	decodeResult(t, call(t, session, "map_entry_update", map[string]any{"entry_id": created.ID, "expected_revision": 2, "idempotency_key": "clear", "changes": map[string]any{"description": ""}}), &updated)
	if updated.Revision != 3 {
		t.Fatal(updated)
	}
}
