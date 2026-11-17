package projectmap_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestMapEditorialOversizedStoredStateReturnsSafeUnavailableWithoutWrites(t *testing.T) {
	for _, mode := range []string{"first_page", "same_position_cursor", "later_position_cursor", "lookahead_probe"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			for i := range 2 {
				entry := workstream()
				if mode != "same_position_cursor" {
					entry.Position = int64(i)
				}
				f.create(fmt.Sprintf("entry-%d", i), entry)
			}
			endpoint := "/api/v1/projects/" + f.project + "/map/editorial"
			w := f.call("GET", endpoint+"?limit=2", f.reader, "", nil)
			expect(t, w, 200)
			var full projectmap.Page[projectmap.Entry]
			decode(t, w, &full)
			if len(full.Items) != 2 || full.HasMore {
				t.Fatal("fixture must have two ordered entries", full)
			}
			target, valid := full.Items[1], full.Items[0]
			path := endpoint + "?limit=1"
			if mode == "first_page" {
				target, valid = valid, target
			} else if mode != "lookahead_probe" {
				w = f.call("GET", path, f.reader, "", nil)
				expect(t, w, 200)
				var first projectmap.Page[projectmap.Entry]
				decode(t, w, &first)
				if !first.HasMore || first.Cursor == "" {
					t.Fatal("fixture must provide a real continuation cursor", first)
				}
				path += "&cursor=" + url.QueryEscape(first.Cursor)
			}
			// Deliberately damage retained storage; normal admission rejects it.
			// SQLite character length is small enough while UTF-8 bytes exceed
			// the full-entry bound, including when the row is only a probe.
			target.Description = strings.Repeat("界", 32000)
			raw, err := json.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}
			if utf8.RuneCount(raw) >= 64<<10 || len(raw) <= 96000 {
				t.Fatal("fixture must distinguish character and byte bounds")
			}
			if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE project_map_entries SET state_json=? WHERE project_id=? AND id=?`, string(raw), f.project, target.ID); err != nil {
				t.Fatal(err)
			}
			before := f.importState()
			w = f.call("GET", path, f.reader, "", nil)
			if w.Code != 503 {
				t.Errorf("oversized editorial state status=%d, want503: %s", w.Code, w.Body.String())
			}
			want := fmt.Sprintf("{\"error\":{\"code\":\"storage_unavailable\",\"message\":\"Stored project-map state is unavailable.\"},\"request_id\":%q}\n", w.Header().Get("X-Request-ID"))
			if w.Header().Get("X-Request-ID") == "" || w.Body.String() != want {
				t.Errorf("editorial read must return the exact safe storage error: %s", w.Body.String())
			}
			// Other stored content and the unchanged retained revision remain usable.
			expect(t, f.call("GET", f.base()+"/"+valid.ID, f.reader, "", nil), 200)
			expect(t, f.call("GET", f.base()+"/"+target.ID+"/history/1", f.reader, "", nil), 200)
			expect(t, f.call("GET", f.base(), f.reader, "", nil), 200)
			if !reflect.DeepEqual(before, f.importState()) {
				t.Fatal("read changed retained entries, history, receipts, events or other application state")
			}
		})
	}
}
