package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestProjectMapPartialCLIActualAPIAndStrictFiles(t *testing.T) {
	f := mapCLIService(t)
	p := f.project(t, "partial-cli")
	worker := f.worker(t, p, "Builder")
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
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "chosen", args...)
		if code != 0 || err != "" {
			t.Fatalf("command %v failed %d %s %s", args, code, out, err)
		}
		return out
	}
	file := mapCLIFile(t, map[string]any{"kind": "workstream", "title": "Area", "description": "PRIVATE_UNCHANGED", "phases": []any{map[string]any{"id": "first", "name": "First"}}})
	var created client.ProjectMapReceipt
	if json.Unmarshal([]byte(invoke("map", "create", "--file", file, "--key", "create")), &created) != nil {
		t.Fatal("create receipt")
	}
	partial := mapCLIFile(t, map[string]any{"next": "Next milestone"})
	args := []string{"map", "update", created.ID, "--file", partial, "--expect-revision", "1", "--key", "update"}
	var updated client.ProjectMapReceipt
	out := invoke(args...)
	if json.Unmarshal([]byte(out), &updated) != nil || updated.Revision != 2 || updated.Outcome != "updated" || strings.Contains(out, "Next milestone") || len(out) > 512 {
		t.Fatal(out)
	}
	if reads.Load() != 0 || writes.Load() != 1 {
		t.Fatal("client-side read/modify/write", reads.Load(), writes.Load())
	}
	before := mapLedgerCounts(t, f)
	if invoke(args...) != out || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("replay changed state")
	}
	code, _, err := mutationInvoke(t, path, "chosen", "map", "update", created.ID, "--file", partial, "--expect-revision", "1", "--key", "stale")
	if code == 0 || !strings.Contains(err, "revision_conflict") {
		t.Fatal(code, err)
	}
	var current client.ProjectMapEntry
	if json.Unmarshal([]byte(invoke("map", "read", created.ID)), &current) != nil || current.Description != "PRIVATE_UNCHANGED" || current.Next != "Next milestone" || len(current.Phases) != 1 {
		t.Fatal(current)
	}
	before = mapLedgerCounts(t, f)
	beforeWrites := writes.Load()
	for i, raw := range []string{`{}`, `{"state":null}`, `{"kind":"section"}`, `{"state":"a","state":"b"}`, `{"links":null}`} {
		invalid := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(invalid, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		code, _, err := mutationInvoke(t, path, "chosen", "map", "update", created.ID, "--file", invalid, "--expect-revision", "2", "--key", string(rune('a'+i)))
		if code == 0 || !strings.Contains(err, "invalid_map_file") {
			t.Fatal("file admitted", raw, code, err)
		}
	}
	if writes.Load() != beforeWrites || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("invalid files dispatched")
	}
}
