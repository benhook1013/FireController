package projectmap_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestMapImportMissingLatestHistoryReturnsSafeUnavailableWithoutWrites(t *testing.T) {
	f := setup(t)
	first := f.previewImport(importDocument())
	f.applyImport(first, "import-original")
	next := first.Document
	next.Source.Snapshot = "snapshot-two"
	f.applyImport(f.previewImport(next), "import-next-source")
	latest := f.importRecord(0)
	if latest.Revision != 2 {
		t.Fatal("fixture needs independently retained older history")
	}
	// Prepare a valid, unused apply before damaging storage, so both write
	// planning and preview must consult the retained source pointer.
	next.Source.Snapshot = "snapshot-three"
	prepared := f.previewImport(next)
	// Simulate damaged retained storage; normal writes enforce the history
	// foreign keys. Restore enforcement before exercising any HTTP request.
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	result, deleteErr := conn.ExecContext(t.Context(), `DELETE FROM project_map_import_history WHERE project_id=? AND source_key=? AND revision=?`, f.project, latest.SourceKey, latest.Revision)
	_, restoreErr := conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	closeErr := conn.Close()
	if deleteErr != nil || restoreErr != nil || closeErr != nil {
		t.Fatal("damage fixture and restore foreign-key enforcement", deleteErr, restoreErr, closeErr)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatal("fixture must remove exactly the latest history record", rows, err)
	}
	before := f.importState()
	path := f.importPath() + "s/selected-history"
	for _, request := range []struct {
		name, method, path, key string
		body                    any
	}{
		{"latest", "GET", path, "", nil},
		{"preview", "POST", f.importPath() + "/preview", "", prepared.Document},
		{"apply", "POST", f.importPath(), "import-after-corruption", projectmap.ImportApplyRequest{Document: prepared.Document, PreviewSHA256: prepared.PreviewSHA256}},
	} {
		t.Run(request.name, func(t *testing.T) {
			w := f.call(request.method, request.path, f.admin, request.key, request.body)
			want := fmt.Sprintf("{\"error\":{\"code\":\"storage_unavailable\",\"message\":\"Stored project-map state is unavailable.\"},\"request_id\":%q}\n", w.Header().Get("X-Request-ID"))
			if w.Code != 503 || w.Header().Get("X-Request-ID") == "" || w.Body.String() != want {
				t.Errorf("missing retained history lost safe classification: status%d body%s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(before, f.importState()) {
				t.Fatal("failed request changed retained rows, receipts or events")
			}
		})
	}
	var failure *core.Error
	if err := f.validate(); !errors.As(err, &failure) || failure.Code != "storage_unavailable" || failure.Status != 503 {
		t.Errorf("snapshot corruption lost domain classification: %v", err)
	}
	expect(t, f.call("GET", path+"/history/1", f.admin, "", nil), 200)
	expect(t, f.call("GET", path+"/history/99", f.admin, "", nil), 404)
	expect(t, f.call("GET", f.importPath()+"s/unknown-source", f.admin, "", nil), 404)
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("failed reads/planning or validation changed retained rows, receipts or events")
	}
}

func TestMapImportStoredNullRecordReturnsSafeUnavailableWithoutWrites(t *testing.T) {
	for _, mode := range []string{"current_pointer_mismatch", "oversized_current", "oversized_historical"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			first := f.previewImport(importDocument())
			f.applyImport(first, "import-original")
			next := first.Document
			next.Source.Snapshot = "snapshot-two"
			f.applyImport(f.previewImport(next), "import-next-source")
			if err := f.validate(); err != nil {
				t.Fatal("valid retained fixture", err)
			}
			latest := f.importRecord(0)
			if latest.Revision != 2 {
				t.Fatal("fixture needs independently retained older revision")
			}
			path := f.importPath() + "s/selected-history"
			goodPath := path + "/history/1"
			if mode == "current_pointer_mismatch" {
				latest.Document.Source.Snapshot = "pointer-does-not-match-history"
				raw, err := json.Marshal(latest)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_map_import_sources SET record_json=? WHERE project_id=? AND source_key=?`, string(raw), f.project, latest.SourceKey); err != nil {
					t.Fatal(err)
				}
				goodPath = path + "/history/2"
			} else {
				damaged := latest
				revision := int64(2)
				if mode == "oversized_historical" {
					damaged = f.importRecord(1)
					revision = 1
					path += "/history/1"
					goodPath = f.importPath() + "s/selected-history"
				}
				// Valid JSON and fewer than the byte-limit number of Unicode
				// characters: SQLite must apply its BLOB byte admission bound.
				damaged.Document.Source.Scope = strings.Repeat("界", projectmap.MaxImportRecordBytes/3)
				raw, err := json.Marshal(damaged)
				if err != nil || len(raw) <= projectmap.MaxImportRecordBytes || utf8.RuneCount(raw) >= projectmap.MaxImportRecordBytes {
					t.Fatal("fixture must distinguish byte/character bounds", err)
				}
				if _, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_map_import_history SET record_json=? WHERE project_id=? AND source_key=? AND revision=?`, string(raw), f.project, latest.SourceKey, revision); err != nil {
					t.Fatal(err)
				}
				if mode == "oversized_current" {
					if _, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_map_import_sources SET record_json=? WHERE project_id=? AND source_key=?`, string(raw), f.project, latest.SourceKey); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := f.importState()
			w := f.call("GET", path, f.admin, "", nil)
			want := fmt.Sprintf("{\"error\":{\"code\":\"storage_unavailable\",\"message\":\"Stored project-map state is unavailable.\"},\"request_id\":%q}\n", w.Header().Get("X-Request-ID"))
			if w.Code != 503 || w.Header().Get("X-Request-ID") == "" || w.Body.String() != want {
				t.Errorf("stored corruption lost safe domain classification: status%d body%s", w.Code, w.Body.String())
			}
			var failure *core.Error
			if err := f.validate(); !errors.As(err, &failure) || failure.Code != "storage_unavailable" || failure.Status != 503 {
				t.Errorf("snapshot corruption lost domain classification: %v", err)
			}
			expect(t, f.call("GET", goodPath, f.admin, "", nil), 200)
			expect(t, f.call("GET", f.importPath()+"s/selected-history/history/99", f.admin, "", nil), 404)
			expect(t, f.call("GET", f.importPath()+"s/unknown-source", f.admin, "", nil), 404)
			if !reflect.DeepEqual(before, f.importState()) {
				t.Fatal("failed reads/snapshot validation changed retained rows, receipts or events")
			}
		})
	}
}

func TestMapImportOversizedCapturedEntryHistoryReturnsSafeUnavailableWithoutWrites(t *testing.T) {
	f := setup(t)
	f.applyImport(f.previewImport(importDocument()), "import-captured-entry")
	imported := f.importRecord(0)
	id := imported.Mappings[0].EntryID
	original := f.read(id)
	next := imported.Document.Entries[0].Entry
	next.Description = "Current editable map content stays available."
	expect(t, f.replace(id, "manual-entry-update", 1, next), 200)
	// Only the captured old entry is oversized; current map/import reads are
	// still valid. Full inspection must classify its bounded NULL projection.
	original.Description = strings.Repeat("界", 32000)
	raw, err := json.Marshal(original)
	if err != nil || len(raw) <= 96000 || utf8.RuneCount(raw) >= 64<<10 {
		t.Fatal("fixture must distinguish byte/character bounds", err)
	}
	if _, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_map_entry_history SET state_json=? WHERE project_id=? AND entry_id=? AND revision=1`, string(raw), f.project, id); err != nil {
		t.Fatal(err)
	}
	before := f.importState()
	var failure *core.Error
	if err := f.validate(); !errors.As(err, &failure) || failure.Code != "storage_unavailable" || failure.Status != 503 {
		t.Errorf("captured entry snapshot lost domain classification: %v", err)
	}
	w := f.call("GET", f.base()+"/"+id+"/history/1", f.reader, "", nil)
	want := fmt.Sprintf("{\"error\":{\"code\":\"storage_unavailable\",\"message\":\"Stored project-map state is unavailable.\"},\"request_id\":%q}\n", w.Header().Get("X-Request-ID"))
	if w.Code != 503 || w.Body.String() != want {
		t.Errorf("captured entry read lost safe response: status%d body%s", w.Code, w.Body.String())
	}
	expect(t, f.call("GET", f.base()+"/"+id, f.reader, "", nil), 200)
	expect(t, f.call("GET", f.importPath()+"s/selected-history", f.admin, "", nil), 200)
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("inspection/read changed retained entry/import rows, receipts or events")
	}
}
