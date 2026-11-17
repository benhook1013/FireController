package projectmap_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestMapPartialUpdatePreservesContentHistoryAndReplay(t *testing.T) {
	f := setup(t)
	original := f.create("original", workstream())
	before := f.read(original.ID)
	path := f.base() + "/" + original.ID
	patch := func(key string, rev int64, changes any) *httptest.ResponseRecorder {
		return f.call("PATCH", path, f.token, key, map[string]any{"expected_revision": rev, "changes": changes})
	}
	first := patch("first", 1, map[string]any{"state": "proof complete"})
	expect(t, first, 200)
	var receipt projectmap.Receipt
	decode(t, first, &receipt)
	if receipt.Revision != 2 || receipt.Outcome != "updated" {
		t.Fatal(receipt)
	}
	current := f.read(original.ID)
	want := before
	want.State = "proof complete"
	want.Revision = 2
	want.UpdatedAt = current.UpdatedAt
	if !reflect.DeepEqual(current, want) {
		t.Fatalf("omitted content changed: %#v", current)
	}
	counts := f.counts()
	unchanged := patch("unchanged", 2, map[string]any{"title": "  " + current.Title + "  "})
	expect(t, unchanged, 200)
	decode(t, unchanged, &receipt)
	after := f.counts()
	if receipt.Outcome != "unchanged" || receipt.Revision != 2 || !reflect.DeepEqual(counts[:5], after[:5]) || after[5] != counts[5]+1 {
		t.Fatalf("no-op effects: %#v %v %v", receipt, counts, after)
	}
	clear := patch("clear", 2, map[string]any{"description": "", "links": []any{}, "position": 0, "archived": false})
	expect(t, clear, 200)
	current = f.read(original.ID)
	if current.Description != "" || len(current.Links) != 0 || len(current.Phases) != 2 || current.State != "proof complete" || current.Revision != 3 {
		t.Fatal(current)
	}
	counts = f.counts()
	// An original-key replay bypasses the now-stale guard but adds no effects.
	replay := patch("first", 1, map[string]any{"state": "proof complete"})
	expect(t, replay, 200)
	if replay.Body.String() != first.Body.String() || !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("replay changed receipt or state")
	}
	stale := patch("stale", 2, map[string]any{"state": "proof complete"})
	expect(t, stale, 409)
	if !strings.Contains(stale.Body.String(), `"current_revision":3`) {
		t.Fatal(stale.Body.String())
	}
	expect(t, patch("first", 1, map[string]any{"state": "other"}), 409)
	history := f.call("GET", path+"/history/1", f.reader, "", nil)
	expect(t, history, 200)
	var captured projectmap.Revision
	decode(t, history, &captured)
	if !reflect.DeepEqual(captured.State, before) {
		t.Fatal("partial edit rewrote history")
	}
	if !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("refused edits wrote effects")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMapPartialUpdateStrictRefusalsScopeAndPhaseRetention(t *testing.T) {
	f := setup(t)
	created := f.create("original", workstream())
	path := f.base() + "/" + created.ID
	before := f.counts()
	invalid := []string{
		`{}`, `{"expected_revision":1}`, `{"expected_revision":null,"changes":{"state":"x"}}`,
		`{"expected_revision":1,"changes":{}}`, `{"expected_revision":1,"changes":null}`,
		`{"expected_revision":1,"changes":{"kind":"section"}}`,
		`{"expected_revision":1,"changes":{"State":"x"}}`,
		`{"expected_revision":1,"changes":{"state":"x","state":"y"}}`,
		`{"expected_revision":1,"changes":{"state":null}}`,
		`{"expected_revision":1,"changes":{"title":" "}}`,
		`{"expected_revision":1,"changes":{"phases":null}}`,
		`{"expected_revision":1,"changes":{"links":null}}`,
		`{"expected_revision":1,"changes":{"phases":[]}}`,
		`{"expected_revision":1,"changes":{"phases":[{"id":"phase_one","name":"One","state":null},{"id":"phase_two","name":"Two"}]}}`,
		`{"expected_revision":1,"changes":{"phases":[{"id":"phase_one","name":"One","unknown":true},{"id":"phase_two","name":"Two"}]}}`,
		`{"expected_revision":1,"changes":{"links":[{"label":"x","url":"javascript:bad"}]}}`,
		`{"expected_revision":1,"changes":{"trigger":"wrong kind"}}`,
		`{"expected_revision":1,"changes":{"position":1000001}}`,
		`{"expected_revision":1,"changes":{"archived":null}}`,
		`{"expected_revision":1,"changes":{"description":"\ud800"}}`,
	}
	for i, raw := range invalid {
		t.Run(fmt.Sprint(i), func(t *testing.T) { expect(t, f.raw("PATCH", path, f.token, fmt.Sprint("bad-", i), []byte(raw)), 400) })
	}
	tooLong, _ := json.Marshal(map[string]any{"expected_revision": 1, "changes": map[string]any{"description": strings.Repeat("x", 8193)}})
	expect(t, f.raw("PATCH", path, f.token, "oversized", tooLong), 400)
	body := map[string]any{"expected_revision": 1, "changes": map[string]any{"state": "x"}}
	expect(t, f.call("PATCH", path, f.reader, "read-only", body), 403)
	// Existing and absent foreign IDs have the same scoped not-found result.
	foreign := "/api/v1/projects/" + f.other + "/map/entries/" + created.ID
	expect(t, f.call("PATCH", foreign, f.admin, "foreign", body), 404)
	expect(t, f.call("PATCH", f.base()+"/map_entry_"+strings.Repeat("a", 32), f.token, "absent", body), 404)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("refusals mutated state")
	}
	// Phase replacement can reorder/rename/archive but retains stable IDs.
	phases := []projectmap.Phase{{ID: "phase_two", Name: "  Renamed  ", Archived: true}, {ID: "phase_one", Name: "One"}}
	good := f.call("PATCH", path, f.token, "phases", map[string]any{"expected_revision": 1, "changes": map[string]any{"phases": phases}})
	expect(t, good, 200)
	result := f.read(created.ID)
	if result.Phases[0].Name != "Renamed" || !result.Phases[0].Archived || result.Phases[1].ID != "phase_one" || result.Description == "" {
		t.Fatal(result)
	}
}

func TestMapPartialUpdateMergedBudgetAndAtomicRollback(t *testing.T) {
	f := setup(t)
	value := workstream()
	value.Description = strings.Repeat("d", 7000)
	value.Phases = make([]projectmap.Phase, 64)
	for i := range value.Phases {
		value.Phases[i] = projectmap.Phase{ID: fmt.Sprintf("phase_%d", i), Name: strings.Repeat("n", 160), State: strings.Repeat("s", 240)}
	}
	value.Links = make([]projectmap.Link, 12)
	for i := range value.Links {
		value.Links[i] = projectmap.Link{Label: "Reference", URL: "https://example.test/" + strings.Repeat("x", 2000)}
	}
	original := f.create("dense", value)
	current := f.read(original.ID)
	counts := f.counts()
	body := map[string]any{"expected_revision": 1, "changes": map[string]any{"where": strings.Repeat("w", 8192), "next": strings.Repeat("n", 8192)}}
	expect(t, f.call("PATCH", f.base()+"/"+original.ID, f.token, "merged-budget", body), 400)
	if !reflect.DeepEqual(current, f.read(original.ID)) || !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("oversized merged value mutated state")
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_patch_event BEFORE INSERT ON events WHEN NEW.event_type LIKE 'map.%' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.call("PATCH", f.base()+"/"+original.ID, f.token, "rollback", map[string]any{"expected_revision": 1, "changes": map[string]any{"state": "Changed"}})
	if failed.Code < 500 {
		t.Fatal("event insertion should fail", failed.Code, failed.Body.String())
	}
	if !reflect.DeepEqual(current, f.read(original.ID)) || !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("failed event retained state/history/receipt")
	}
}
