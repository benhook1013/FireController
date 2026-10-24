package projectmap_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestMapEditorialPagesFullBodiesAndReadConservation(t *testing.T) {
	f := setup(t)
	var want []projectmap.Entry
	for i, pos := range []int64{90, 0, 7, 7, 1000000, 2, 0} {
		v := workstream()
		v.Title = fmt.Sprintf("Entry %d", i)
		v.Position = pos
		r := f.create(fmt.Sprintf("editorial-%d", i), v)
		want = append(want, f.read(r.ID))
	}
	archived := workstream()
	archived.Archived = true
	f.create("archived-editorial", archived)
	sort.Slice(want, func(i, j int) bool {
		if want[i].Position != want[j].Position {
			return want[i].Position < want[j].Position
		}
		return want[i].ID < want[j].ID
	})
	endpoint := "/api/v1/projects/" + f.project + "/map/editorial"
	before := f.counts()
	w := f.call("GET", endpoint, f.reader, "", nil)
	expect(t, w, 200)
	var first projectmap.Page[projectmap.Entry]
	decode(t, w, &first)
	if len(first.Items) != 5 || !first.HasMore || first.Cursor == "" || !reflect.DeepEqual(first.Items, want[:5]) {
		t.Fatal(first)
	}
	var actual []projectmap.Entry
	cursor := ""
	for {
		query := "?limit=2"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		w = f.call("GET", endpoint+query, f.reader, "", nil)
		expect(t, w, 200)
		var page projectmap.Page[projectmap.Entry]
		decode(t, w, &page)
		if page.Items == nil || len(page.Items) > 2 {
			t.Fatal(page)
		}
		actual = append(actual, page.Items...)
		if !page.HasMore {
			if page.Cursor != "" {
				t.Fatal(page)
			}
			break
		}
		cursor = page.Cursor
	}
	if !reflect.DeepEqual(want, actual) {
		t.Fatalf("editorial sequence mismatch: got %v want %v", actual, want)
	}
	// The existing compact list keeps its original ID ordering and omits bodies.
	w = f.call("GET", f.base(), f.reader, "", nil)
	expect(t, w, 200)
	var compact projectmap.Page[projectmap.Summary]
	decode(t, w, &compact)
	if len(compact.Items) != len(want) || strings.Contains(w.Body.String(), "description") {
		t.Fatal(w.Body.String())
	}
	for i := 1; i < len(compact.Items); i++ {
		if compact.Items[i-1].ID >= compact.Items[i].ID {
			t.Fatal(compact)
		}
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("read added durable effects")
	}
	// Cursor scope binds the principal, project, and exact ordering.
	expect(t, f.call("GET", endpoint+"?cursor="+url.QueryEscape(first.Cursor), f.token, "", nil), 400)
	expect(t, f.call("GET", f.base()+"?cursor="+url.QueryEscape(first.Cursor), f.reader, "", nil), 400)
	worker := f.worker
	f.worker = ""
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/map/editorial?cursor="+url.QueryEscape(first.Cursor), f.admin, "", nil), 400)
	f.worker = worker
	// Signed but malformed payloads must not silently become the first page.
	req := httptest.NewRequest("GET", endpoint, nil)
	req.SetPathValue("project_id", f.project)
	req.Header.Set("Authorization", "Bearer "+f.reader)
	req.Header.Set("X-FC-Instance", f.store.InstanceID())
	req.Header.Set("X-FC-Worker", f.worker)
	scope, err := f.store.Scope(req, "read")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"position":null,"id":"` + want[0].ID + `"}`, `{"position":-1,"id":"` + want[0].ID + `"}`, `{"position":1,"id":"bad"}`, `{"position":0,"id":"` + want[0].ID + `","extra":1}`} {
		bad := f.store.Cursor(scope, "map.editorial:position-id-asc:v1", raw)
		expect(t, f.call("GET", endpoint+"?cursor="+url.QueryEscape(bad), f.reader, "", nil), 400)
	}
	for _, query := range []string{"limit=0", "limit=11", "limit=2&limit=3", "kind=section", "archived=false", "cursor=bad", "cursor=", "worker=x"} {
		expect(t, f.call("GET", endpoint+"?"+query, f.reader, "", nil), 400)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("refused read added durable effects")
	}
}

func TestMapPositionCanonicalBytesHistoryAndExactReplay(t *testing.T) {
	f := setup(t)
	v := workstream()
	original := f.create("position-original", v)
	var old string
	if err := f.store.DB().QueryRow("SELECT state_json FROM project_map_entries WHERE project_id=? AND id=?", f.project, original.ID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(old, `"position"`) {
		t.Fatal("zero changed legacy canonical bytes")
	}
	explicit, _ := json.Marshal(v)
	explicit = append(explicit[:len(explicit)-1], []byte(`,"position":0}`)...)
	retry := f.raw("POST", f.base(), f.token, "position-original", explicit)
	expect(t, retry, 201)
	var replay projectmap.Receipt
	decode(t, retry, &replay)
	if replay != original {
		t.Fatal(replay)
	}
	v.Position = 1000000
	changed := f.replace(original.ID, "position-move", 1, v)
	expect(t, changed, 200)
	if f.read(original.ID).Position != 1000000 {
		t.Fatal("position was not persisted")
	}
	noOp := f.replace(original.ID, "position-noop", 2, v)
	expect(t, noOp, 200)
	var noop projectmap.Receipt
	decode(t, noOp, &noop)
	if noop.Outcome != "unchanged" || noop.Revision != 2 {
		t.Fatal(noop)
	}
	before := f.counts()
	expect(t, f.raw("POST", f.base(), f.token, "position-original", explicit), 201)
	if !reflect.DeepEqual(before, f.counts()) || f.read(original.ID).Position != 1000000 {
		t.Fatal("old replay altered moved entry")
	}
	w := f.call("GET", f.base()+"/"+original.ID+"/history/1", f.reader, "", nil)
	expect(t, w, 200)
	var historical projectmap.Revision
	decode(t, w, &historical)
	encoded, _ := json.Marshal(historical.State)
	if string(encoded) != old || historical.State.Position != 0 {
		t.Fatal("legacy history bytes changed")
	}
	if err := f.validate(); err != nil {
		t.Fatal("canonical snapshot/import compatibility", err)
	}
}

func TestMapSectionAndPositionValidation(t *testing.T) {
	f := setup(t)
	before := f.counts()
	for i, raw := range []string{`{"kind":"section","title":"x","position":null}`, `{"kind":"section","title":"x","position":-1}`, `{"kind":"section","title":"x","position":1000001}`, `{"kind":"section","title":"x","position":1.5}`, `{"kind":"section","title":"x","where":"x"}`, `{"kind":"section","title":"x","next":"x"}`, `{"kind":"section","title":"x","trigger":"x"}`, `{"kind":"section","title":"x","phases":[{"id":"p","name":"x"}]}`} {
		expect(t, f.raw("POST", f.base(), f.token, fmt.Sprintf("bad-section-%d", i), []byte(raw)), 400)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("invalid input wrote")
	}
	v := projectmap.Editable{Kind: "section", Title: "Narrative", Description: "# Heading\n\nLiteral <script>markup</script> and **bold**", State: "Draft", Position: 4, Links: []projectmap.Link{{Label: "Spec", URL: "https://example.test/spec"}}}
	created := f.create("section", v)
	current := f.read(created.ID)
	if current.Kind != "section" || current.Description != v.Description || current.Position != 4 || current.Phases == nil {
		t.Fatal(current)
	}
	w := f.call("GET", f.base()+"?kind=section", f.reader, "", nil)
	expect(t, w, 200)
	var page projectmap.Page[projectmap.Summary]
	decode(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatal(page)
	}
	doc := importDocument()
	doc.Entries[0].Entry = v
	imported := f.applyImport(f.previewImport(doc), "section-import")
	if imported.Created != 1 {
		t.Fatal(imported)
	}
	record := f.importRecord(0)
	if record.Document.Entries[0].Entry.Position != 4 || record.Document.Entries[0].Entry.Kind != "section" {
		t.Fatal(record)
	}
	v.Kind = "return_point"
	expect(t, f.replace(created.ID, "section-kind-change", 1, v), 400)
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMapEditorialEmptyPage(t *testing.T) {
	f := setup(t)
	v := workstream()
	v.Archived = true
	f.create("archived-only", v)
	w := f.call("GET", "/api/v1/projects/"+f.project+"/map/editorial", f.reader, "", nil)
	expect(t, w, 200)
	var page projectmap.Page[projectmap.Entry]
	decode(t, w, &page)
	if page.Items == nil || len(page.Items) != 0 || page.HasMore || page.Cursor != "" {
		t.Fatal(page)
	}
}
