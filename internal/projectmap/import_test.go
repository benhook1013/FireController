package projectmap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/projectmap"
)

func importDocument() projectmap.ImportDocument {
	e := workstream()
	e.Title = "  Selected track  "
	return projectmap.ImportDocument{Version: 1, Source: projectmap.ImportSource{Key: "selected-history", RepositoryID: 1, Kind: "prepared", Snapshot: "snapshot-one", CollectedAt: "2026-10-17T13:00:00+13:00", TransformationVersion: 1, Scope: "Two deliberately selected sources"}, Entries: []projectmap.ImportItem{{SourceKey: "track-one", ExpectedRevision: 0, Entry: e, Provenance: projectmap.ImportProvenance{References: []string{"https://example.invalid/source#section"}, Inferred: true, Confidence: "medium", Uncertainty: "Private inferred proposal, not verified completion <literal>"}}}}
}
func (f *fixture) importPath() string { return "/api/v1/projects/" + f.project + "/map/import" }
func (f *fixture) previewImport(doc projectmap.ImportDocument) projectmap.ImportPreview {
	f.t.Helper()
	w := f.call("POST", f.importPath()+"/preview", f.admin, "", doc)
	expect(f.t, w, 200)
	var out projectmap.ImportPreview
	decode(f.t, w, &out)
	return out
}
func (f *fixture) applyImport(preview projectmap.ImportPreview, key string) projectmap.ImportReceipt {
	f.t.Helper()
	w := f.call("POST", f.importPath(), f.admin, key, projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256})
	expect(f.t, w, 200)
	var out projectmap.ImportReceipt
	decode(f.t, w, &out)
	return out
}
func (f *fixture) importRecord(rev int64) projectmap.ImportRecord {
	f.t.Helper()
	path := f.importPath() + "s/selected-history"
	if rev > 0 {
		path += fmt.Sprintf("/history/%d", rev)
	}
	w := f.call("GET", path, f.admin, "", nil)
	expect(f.t, w, 200)
	var out projectmap.ImportRecord
	decode(f.t, w, &out)
	return out
}

// Inspect every application table to prove preview/failure conservation, not
// merely the domain currently under test. Synthetic fixture data only.
func (f *fixture) importState() map[string]string {
	f.t.Helper()
	tables, err := f.store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		f.t.Fatal(err)
	}
	names := []string{}
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			f.t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		f.t.Fatal(err)
	}
	tables.Close()
	state := map[string]string{}
	for _, name := range names {
		rows, err := f.store.DB().Query(`SELECT * FROM "` + strings.ReplaceAll(name, `"`, `""`) + `" ORDER BY rowid`)
		if err != nil {
			f.t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			f.t.Fatal(err)
		}
		values := [][]any{}
		for rows.Next() {
			row := make([]any, len(columns))
			dest := make([]any, len(columns))
			for n := range row {
				dest[n] = &row[n]
			}
			if err := rows.Scan(dest...); err != nil {
				f.t.Fatal(err)
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
		rows.Close()
		raw, err := json.Marshal(values)
		if err != nil {
			f.t.Fatal(err)
		}
		state[name] = string(raw)
	}
	return state
}
func TestMapImportPreviewConservationNormalizationAndBinding(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	before := f.importState()
	preview := f.previewImport(doc)
	if !preview.Ready || preview.Conflicts != 0 || len(preview.PreviewSHA256) != 64 || len(preview.Plan) != 1 || preview.Plan[0].Action != "create" || preview.Plan[0].EntryID != "" || preview.Plan[0].CurrentRevision != 0 {
		t.Fatal("initial preview plan")
	}
	if preview.Document.Source.CollectedAt != "2026-10-17T00:00:00Z" || preview.Document.Entries[0].Entry.Title != "Selected track" || preview.Document.Entries[0].Provenance.Uncertainty != doc.Entries[0].Provenance.Uncertainty {
		t.Fatal("normalization changed source meaning")
	}
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("preview mutated application state")
	}
	equivalent := preview.Document
	if f.previewImport(equivalent).PreviewSHA256 != preview.PreviewSHA256 {
		t.Fatal("normalized input changed binding")
	}
	for _, change := range []string{"document", "project", "worker"} {
		t.Run(change, func(t *testing.T) {
			request := projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256}
			path := f.importPath()
			oldWorker := f.worker
			switch change {
			case "document":
				request.Document.Source.Snapshot = "different"
			case "project":
				path = "/api/v1/projects/" + f.other + "/map/import"
				f.worker = ""
			case "worker":
				f.worker = ""
			}
			status := 409
			if change == "project" || change == "worker" {
				status = 400
			} // explicit worker admission precedes hash checks
			expect(t, f.call("POST", path, f.admin, "changed-"+change, request), status)
			f.worker = oldWorker
		})
	}
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("mismatched apply mutated state")
	}
	wrong := doc
	wrong.Source.RepositoryID = 2
	expect(t, f.call("POST", f.importPath()+"/preview", f.admin, "", wrong), 400)
}
func TestMapImportStableMappingProvenanceOmissionReplayAndHistory(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	second := doc.Entries[0]
	second.SourceKey = "track-two"
	second.Entry.Title = "Second selected track"
	doc.Entries = append(doc.Entries, second)
	first := f.previewImport(doc)
	receipt := f.applyImport(first, "initial-import")
	if receipt.Revision != 1 || receipt.Outcome != "imported" || receipt.Created != 2 || receipt.Updated != 0 || receipt.Unchanged != 0 {
		t.Fatal(receipt)
	}
	original := f.importRecord(0)
	if len(original.Mappings) != 2 || original.ActorID == "" || original.WorkerID != f.worker || original.Document.Entries[0].Provenance.Confidence != "medium" || !original.Document.Entries[0].Provenance.Inferred {
		t.Fatal("provenance not retained")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	before := f.importState()
	repeat := f.previewImport(first.Document)
	if repeat.Plan[0].Action != "unchanged" || repeat.Plan[1].Action != "unchanged" {
		t.Fatal("zero revision stable mapping")
	}
	noOp := f.applyImport(repeat, "new-key-same-import")
	if noOp.Outcome != "unchanged" || noOp.Revision != 1 || noOp.Unchanged != 2 {
		t.Fatal(noOp)
	}
	after := f.importState()
	delete(before, "receipts")
	delete(after, "receipts")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate prevention changed state")
	}
	// Manual edits remain independent of source facts and cannot be overwritten
	// by an old revision-zero input.
	manual := original.Document.Entries[1].Entry
	manual.Title = "Manual edit survives omission"
	expect(t, f.replace(original.Mappings[1].EntryID, "manual-map-edit", 1, manual), 200)
	conflict := f.previewImport(first.Document)
	if conflict.Ready || conflict.PreviewSHA256 != "" || conflict.Conflicts != 1 || conflict.Plan[1].Reason != "entry_changed" {
		t.Fatal("manual change hidden by preview")
	}
	next := first.Document
	next.Source.Snapshot = "snapshot-two"
	next.Entries = append([]projectmap.ImportItem{}, next.Entries[:1]...)
	next.Entries[0].ExpectedRevision = 1
	next.Entries[0].Entry.Title = "Updated imported track"
	updated := f.applyImport(f.previewImport(next), "second-import")
	if updated.Revision != 2 || updated.Created != 0 || updated.Updated != 1 {
		t.Fatal(updated)
	}
	latest := f.importRecord(0)
	if len(latest.Mappings) != 1 || latest.Mappings[0].EntryID != original.Mappings[0].EntryID || f.read(original.Mappings[1].EntryID).Title != manual.Title {
		t.Fatal("omission rewrote existing record")
	}
	var rev, sourceRev int64
	if err := f.store.DB().QueryRow(`SELECT entry_revision,source_revision FROM project_map_import_items WHERE item_key='track-two'`).Scan(&rev, &sourceRev); err != nil || rev != 1 || sourceRev != 1 {
		t.Fatal("omitted provenance moved", err)
	}
	history := f.call("GET", f.importPath()+"s/selected-history/history?limit=1", f.admin, "", nil)
	expect(t, history, 200)
	var page projectmap.Page[projectmap.ImportHistorySummary]
	decode(t, history, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore || page.Cursor == "" || strings.Contains(history.Body.String(), "uncertainty") {
		t.Fatal("history not compact")
	}
	expect(t, f.call("GET", f.importPath()+"s/selected-history/history?limit=1&cursor="+url.QueryEscape(page.Cursor), f.admin, "", nil), 200)
	if !reflect.DeepEqual(original, f.importRecord(1)) {
		t.Fatal("historical source facts rewritten")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	// Exact original receipt remains recoverable despite manual and later imports.
	replay := f.applyImport(first, "initial-import")
	if replay != receipt {
		t.Fatal("original replay changed receipt")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.mount()
	if replay := f.applyImport(first, "initial-import"); replay != receipt {
		t.Fatal("restart lost exact recovery")
	}
}
func TestMapImportApplyRechecksConflictsAndRollsBackAllWrites(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	first := f.previewImport(doc)
	f.applyImport(first, "initial")
	changed := first.Document
	changed.Entries[0].ExpectedRevision = 1
	changed.Entries[0].Entry.Title = "Import update"
	preview := f.previewImport(changed)
	manual := first.Document.Entries[0].Entry
	manual.Title = "Concurrent edit"
	id := f.importRecord(0).Mappings[0].EntryID
	expect(t, f.replace(id, "concurrent", 1, manual), 200)
	before := f.importState()
	expect(t, f.call("POST", f.importPath(), f.admin, "stale-import", projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256}), 409)
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("stale import partially wrote")
	}
	changed.Entries[0].ExpectedRevision = 2
	added := changed.Entries[0]
	added.SourceKey = "new-item"
	added.ExpectedRevision = 0
	changed.Entries = append(changed.Entries, added)
	preview = f.previewImport(changed)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_import_history BEFORE INSERT ON project_map_import_history BEGIN SELECT RAISE(ABORT,'synthetic import failure'); END`); err != nil {
		t.Fatal(err)
	}
	before = f.importState()
	expect(t, f.call("POST", f.importPath(), f.admin, "rollback-import", projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256}), 500)
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("failed source history left partial map/provenance/events/receipt")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_import_history`); err != nil {
		t.Fatal(err)
	}
	if result := f.applyImport(preview, "rollback-import"); result.Updated != 1 || result.Created != 1 {
		t.Fatal(result)
	}
	// Invalid kind and omitted retained phases are preview conflicts, not a path
	// around the original map mutation contracts.
	retained := preview.Document
	retained.Entries = append([]projectmap.ImportItem{}, retained.Entries[:1]...)
	retained.Entries[0].ExpectedRevision = 3
	retained.Entries[0].Entry.Phases = []projectmap.Phase{}
	conflict := f.previewImport(retained)
	if conflict.Ready || conflict.Plan[0].Reason != "map_contract_conflict" {
		t.Fatal("retained phases not enforced")
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMapImportProvenanceOnlyRevisionAndChangedKeyConflict(t *testing.T) {
	f := setup(t)
	first := f.previewImport(importDocument())
	initial := f.applyImport(first, "retained-key")
	original := f.importRecord(0)
	changed := first.Document
	changed.Source.Snapshot = "new selected snapshot"
	changed.Entries[0].Provenance.Uncertainty = "Later attribution, unchanged editorial content"
	preview := f.previewImport(changed)
	before := f.importState()
	expect(t, f.call("POST", f.importPath(), f.admin, "retained-key", projectmap.ImportApplyRequest{Document: preview.Document, PreviewSHA256: preview.PreviewSHA256}), 409)
	if !reflect.DeepEqual(before, f.importState()) {
		t.Fatal("changed key reuse wrote state")
	}
	next := f.applyImport(preview, "provenance-only")
	if initial.Revision != 1 || next.Revision != 2 || next.Outcome != "imported" || next.Created != 0 || next.Updated != 0 || next.Unchanged != 1 {
		t.Fatal(next)
	}
	if entry := f.read(original.Mappings[0].EntryID); entry.Revision != 1 {
		t.Fatal("provenance edit pretended map change")
	}
	if f.importRecord(0).Mappings[0].SourceRevision != 2 || f.importRecord(1).Mappings[0].SourceRevision != 1 {
		t.Fatal("source history attribution moved")
	}
	oldWorker := f.worker
	f.worker = ""
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/map/imports/selected-history", f.admin, "", nil), 404)
	f.worker = oldWorker
	expect(t, f.call("GET", f.importPath()+"s/selected-history", f.token, "", nil), 403)
	expect(t, f.call("GET", f.importPath()+"s/selected-history/history", f.reader, "", nil), 403)
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMapImportLatestUpdatedDocumentNewKeyIsNarrowNoOp(t *testing.T) {
	f := setup(t)
	first := f.previewImport(importDocument())
	f.applyImport(first, "first-import")
	doc := first.Document
	doc.Source.Snapshot = "positive-revision-update"
	doc.Entries[0].ExpectedRevision = 1
	doc.Entries[0].Entry.Title = "Approved update"
	updatedPreview := f.previewImport(doc)
	updated := f.applyImport(updatedPreview, "approved-update")
	if updated.Revision != 2 || updated.Updated != 1 {
		t.Fatal(updated)
	}
	before := f.importState()
	repeated := f.previewImport(updatedPreview.Document)
	if !repeated.Ready || repeated.Conflicts != 0 || repeated.Plan[0].Action != "unchanged" || repeated.Plan[0].Reason != "retained_document_unchanged" || repeated.Plan[0].CurrentRevision != 2 {
		t.Fatal("exact retained update was not reusable with a new key", repeated.Ready, repeated.Plan)
	}
	noOp := f.applyImport(repeated, "new-key-exact-update")
	if noOp.Outcome != "unchanged" || noOp.Revision != 2 || noOp.Created != 0 || noOp.Updated != 0 || noOp.Unchanged != 1 {
		t.Fatal(noOp)
	}
	after := f.importState()
	delete(before, "receipts")
	delete(after, "receipts")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retained document retry duplicated domain state")
	}
	// Merely changing provenance/snapshot invalidates the exception: a stale
	// positive guard needs a new explicit current revision even for equal text.
	changed := updatedPreview.Document
	changed.Source.Snapshot = "different-approved-document"
	stale := f.previewImport(changed)
	if stale.Ready || stale.Plan[0].Reason != "revision_changed" {
		t.Fatal("changed stale document bypassed CAS")
	}
	id := f.importRecord(0).Mappings[0].EntryID
	manual := updatedPreview.Document.Entries[0].Entry
	manual.Title = "Manual later edit"
	expect(t, f.replace(id, "manual-later", 2, manual), 200)
	stale = f.previewImport(updatedPreview.Document)
	if stale.Ready || stale.Plan[0].Reason != "revision_changed" {
		t.Fatal("manual edit bypassed retained-document guard")
	}
	expect(t, f.call("POST", f.importPath(), f.admin, "manual-stale-apply", projectmap.ImportApplyRequest{Document: updatedPreview.Document, PreviewSHA256: updatedPreview.PreviewSHA256}), 409)
	// Editing back to identical content must not erase the newer manual revision.
	expect(t, f.replace(id, "manual-edit-back", 3, updatedPreview.Document.Entries[0].Entry), 200)
	stale = f.previewImport(updatedPreview.Document)
	if stale.Ready || stale.Plan[0].CurrentRevision != 4 || stale.Plan[0].Reason != "revision_changed" {
		t.Fatal("edit-back text equality bypassed revision guard")
	}
	expect(t, f.call("POST", f.importPath(), f.admin, "manual-back-stale-apply", projectmap.ImportApplyRequest{Document: updatedPreview.Document, PreviewSHA256: updatedPreview.PreviewSHA256}), 409)
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMapImportRetainedRetryRequiresEveryCapturedTarget(t *testing.T) {
	f := setup(t)
	doc := importDocument()
	second := doc.Entries[0]
	second.SourceKey = "second-track"
	second.Entry.Title = "Second track"
	doc.Entries = append(doc.Entries, second)
	first := f.previewImport(doc)
	f.applyImport(first, "first")
	updated := first.Document
	updated.Source.Snapshot = "both-updated"
	for n := range updated.Entries {
		updated.Entries[n].ExpectedRevision = 1
		updated.Entries[n].Entry.Title = fmt.Sprintf("Updated track %d", n)
	}
	preview := f.previewImport(updated)
	f.applyImport(preview, "both-update")
	repeated := f.previewImport(preview.Document)
	if !repeated.Ready || repeated.Plan[0].Reason != "retained_document_unchanged" || repeated.Plan[1].Reason != "retained_document_unchanged" {
		t.Fatal("whole retained retry not admitted")
	}
	record := f.importRecord(0)
	manual := preview.Document.Entries[1].Entry
	manual.Title = "Manual second track"
	expect(t, f.replace(record.Mappings[1].EntryID, "manual-second", 2, manual), 200)
	repeated = f.previewImport(preview.Document)
	if repeated.Ready || repeated.Conflicts != 2 || repeated.Plan[0].Reason != "revision_changed" || repeated.Plan[1].Reason != "revision_changed" {
		t.Fatal("one changed target partially widened stale guards")
	}
}
