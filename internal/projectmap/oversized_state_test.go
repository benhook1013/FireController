package projectmap_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMapOversizedStoredStateReturnsSafeUnavailableWithoutWrites(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "retained-revision"}[historical], func(t *testing.T) {
			f := setup(t)
			created := f.create("create", workstream())
			original := f.read(created.ID)
			next := workstream()
			next.Description = "later current state"
			expect(t, f.replace(created.ID, "update", 1, next), 200)
			current := f.read(created.ID)
			damaged := current
			path := f.base() + "/" + created.ID
			query := `UPDATE project_map_entries SET state_json=? WHERE project_id=? AND id=?`
			if historical {
				damaged = original
				path += "/history/1"
				query = `UPDATE project_map_entry_history SET state_json=? WHERE project_id=? AND entry_id=? AND revision=1`
			}
			// Character count stays below the editable bound while UTF-8 bytes
			// exceed even the larger full-entry bound. Storage is deliberately
			// damaged directly; admission never accepts this value.
			damaged.Description = strings.Repeat("界", 32000)
			raw, err := json.Marshal(damaged)
			if err != nil {
				t.Fatal(err)
			}
			if utf8.RuneCount(raw) >= 64<<10 || len(raw) <= 96_000 {
				t.Fatal("fixture does not distinguish SQLite character and byte bounds")
			}
			if _, err := f.store.DB().ExecContext(t.Context(), query, string(raw), f.project, created.ID); err != nil {
				t.Fatal(err)
			}
			// Capture exact retained bodies and receipt/event payloads, not only
			// their row counts, before either authenticated read.
			queries := []string{
				`SELECT state_json FROM project_map_entries WHERE project_id=? AND id=?`,
				`SELECT state_json FROM project_map_entry_history WHERE project_id=? AND entry_id=? AND revision=1`,
				`SELECT json_group_array(json_array(instance_id,principal_id,project_id,operation,target,key,fingerprint,response_json)) FROM (SELECT * FROM receipts ORDER BY rowid)`,
				`SELECT json_group_array(json_array(sequence,project_id,event_type,resource_id,revision,actor_id,payload_json,created_at)) FROM (SELECT * FROM events ORDER BY sequence)`,
			}
			retained := func() []string {
				t.Helper()
				values := make([]string, len(queries))
				for i, query := range queries {
					var args []any
					if i < 2 {
						args = []any{f.project, created.ID}
					}
					if err := f.store.DB().QueryRowContext(t.Context(), query, args...).Scan(&values[i]); err != nil {
						t.Fatal(err)
					}
				}
				return values
			}
			before, counts := retained(), f.counts()
			w := f.call("GET", path, f.reader, "", nil)
			if w.Code != 503 {
				t.Errorf("oversized state status=%d, want503: %s", w.Code, w.Body.String())
			}
			want := fmt.Sprintf("{\"error\":{\"code\":\"storage_unavailable\",\"message\":\"Stored project-map state is unavailable.\"},\"request_id\":%q}\n", w.Header().Get("X-Request-ID"))
			if w.Header().Get("X-Request-ID") == "" || w.Body.String() != want {
				t.Errorf("oversized state did not return the exact safe storage error: %s", w.Body.String())
			}
			// Damage is isolated: the other full-body reader still succeeds.
			other := f.base() + "/" + created.ID + "/history/1"
			if historical {
				other = f.base() + "/" + created.ID
			}
			expect(t, f.call("GET", other, f.reader, "", nil), 200)
			expect(t, f.call("GET", f.base()+"/map_entry_00000000000000000000000000000000", f.reader, "", nil), 404)
			expect(t, f.call("GET", f.base()+"/"+created.ID+"/history/99", f.reader, "", nil), 404)
			if !reflect.DeepEqual(before, retained()) || !reflect.DeepEqual(counts, f.counts()) {
				t.Fatal("read changed retained bytes, history, receipts or events")
			}
		})
	}
}
