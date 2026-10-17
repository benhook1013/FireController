package backup

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

// Use actual import admissions and map edits rather than manufacturing source
// records: omitted bindings deliberately retain their original source revision.
func mapImportRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	projectmap.Register(f.handler.(*http.ServeMux), f.store)
	entry := projectmap.Editable{Kind: "workstream", Title: "First approved track", Description: "Private inferred proposal <literal>", State: "Proposed", Phases: []projectmap.Phase{{ID: "foundation", Name: "Foundation", State: "Pending"}}, Links: []projectmap.Link{{Label: "Original source", URL: "https://example.invalid/selected-source"}}}
	item := func(key string, value projectmap.Editable) map[string]any {
		return map[string]any{"source_key": key, "expected_revision": 0, "entry": value, "provenance": map[string]any{"references": []string{"https://example.invalid/source/" + key}, "inferred": true, "confidence": "medium", "uncertainty": "Proposal from selected source only"}}
	}
	first := item("track-a", entry)
	omittedEntry := entry
	omittedEntry.Title = "Original omitted track"
	second := item("track-b", omittedEntry)
	document := map[string]any{"version": 1, "source": map[string]any{"key": "prepared-onboarding", "repository_id": 991, "kind": "prepared", "snapshot": "approved-v1", "collected_at": "2026-10-17T00:00:00Z", "transformation_version": 1, "scope": "Two explicitly selected source tracks"}, "entries": []any{first, second}}
	apply := func(document map[string]any, revision float64) {
		preview := f.request(t, "POST", "/api/v1/projects/"+f.project+"/map/import/preview", document)
		if preview["ready"] != true || preview["preview_sha256"] == "" {
			t.Fatal("fixture import not ready", preview)
		}
		receipt := f.request(t, "POST", "/api/v1/projects/"+f.project+"/map/import", map[string]any{"document": preview["document"], "preview_sha256": preview["preview_sha256"]})
		if receipt["revision"] != revision {
			t.Fatal("fixture source revision", receipt)
		}
	}
	apply(document, 1)
	var omittedID string
	if err := f.store.DB().QueryRow(`SELECT entry_id FROM project_map_import_items WHERE project_id=? AND source_key='prepared-onboarding' AND item_key='track-b'`, f.project).Scan(&omittedID); err != nil {
		t.Fatal(err)
	}
	// An ordinary edit is newer than the last approved import; a later import
	// omits that binding and must preserve both the edit and old provenance.
	omittedEntry.Title = "Manual edit survives omission"
	omittedEntry.Description = "A private manual edit, distinct from imported source facts"
	f.request(t, "PUT", "/api/v1/projects/"+f.project+"/map/entries/"+omittedID, map[string]any{"expected_revision": 1, "entry": omittedEntry})
	entry.Title = "Second approved track title"
	first["expected_revision"] = 1
	first["entry"] = entry
	document["entries"] = []any{first}
	document["source"].(map[string]any)["snapshot"] = "approved-v2"
	apply(document, 2)
	var selected, omitted int
	if err := f.store.DB().QueryRow(`SELECT json_array_length(record_json,'$.mappings') FROM project_map_import_sources`).Scan(&selected); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM project_map_import_items WHERE item_key='track-b' AND source_revision=1 AND entry_revision=1`).Scan(&omitted); err != nil {
		t.Fatal(err)
	}
	if selected != 1 || omitted != 1 {
		t.Fatal("fixture did not preserve an omitted older binding", selected, omitted)
	}
	if _, err := validateSchema(t.Context(), f.store.DB(), false); err != nil {
		t.Fatal("valid imported fixture failed backup schema admission", err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = projectmap.ValidateSnapshot(t.Context(), tx); err != nil {
		t.Fatal("valid imported fixture failed canonical snapshot validation", err)
	}
	return f
}

func TestMapImportBundlePreservesProvenanceOmissionsAndPrivateState(t *testing.T) {
	f := mapImportRestoreFixture(t)
	queries := map[string]string{
		"sources":               `SELECT * FROM project_map_import_sources ORDER BY project_id,source_key`,
		"source-history":        `SELECT * FROM project_map_import_history ORDER BY project_id,source_key,revision`,
		"all-retained-bindings": `SELECT * FROM project_map_import_items ORDER BY project_id,source_key,item_key`,
		"current-map":           `SELECT * FROM project_map_entries ORDER BY project_id,id`,
		"map-history":           `SELECT * FROM project_map_entry_history ORDER BY project_id,entry_id,revision`,
		"private-jobs":          `SELECT * FROM jobs ORDER BY project_id,id`,
		"private-messages":      `SELECT * FROM inbox_messages ORDER BY sequence`,
		"private-reviews":       `SELECT * FROM review_findings ORDER BY project_id,snapshot_id,pull_number,run_id,position`,
	}
	before := map[string][][]any{}
	for name, query := range queries {
		before[name] = restoreRows(t, f.store.DB(), query)
	}
	bundle := restoreBundle(t, f)
	beforeTree := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeTree, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection changed immutable map-import bundle")
	}
	target := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), bundle.Path, target)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
		t.Fatal("restore activated or reused predecessor", restored)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, query := range queries {
		if !reflect.DeepEqual(before[name], restoreRows(t, db, query)) {
			t.Fatal("restore changed retained map/provenance/private state", name)
		}
		if !reflect.DeepEqual(before[name], restoreRows(t, f.store.DB(), query)) {
			t.Fatal("backup, inspection or restore changed live source state", name)
		}
	}
	var version, state string
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:project_map'`).Scan(&version); err != nil || version != "2" {
		t.Fatal("map schema version changed", version, err)
	}
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='restore:state'`).Scan(&state); err != nil || state != "inactive" {
		t.Fatal("restored coordination unexpectedly active", state, err)
	}
}

func TestMapImportBundleV1RemainsReadableWithoutInstallingImportTables(t *testing.T) {
	f, _, _ := mapRestoreFixture(t)
	// Reconstruct an empty-import pre-v2 fixture while retaining genuine v1 map
	// state and history. Inspection must not run the new migration.
	if _, err := f.store.DB().Exec(`DROP TABLE project_map_import_items; DROP TABLE project_map_import_history; DROP TABLE project_map_import_sources; UPDATE metadata SET value='1' WHERE key='schema:project_map'`); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("v1 inspection migrated or modified old map state")
	}
	target := filepath.Join(t.TempDir(), "restored-v1")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name LIKE 'project_map_import_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("v1 restore installed new import tables", tables, err)
	}
}

func TestMapImportBundleRejectsResealedMalformedProvenance(t *testing.T) {
	f := mapImportRestoreFixture(t)
	for name, change := range map[string]string{
		"current-source-key":                `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.source_key','different-source')`,
		"current-revision":                  `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.revision',999)`,
		"current-document":                  `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.document.source.repository_id',992)`,
		"current-mapping":                   `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.mappings[0].entry_revision',999)`,
		"current-missing-mapping":           `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.mappings',json('[]'))`,
		"history-body":                      `UPDATE project_map_import_history SET record_json='{}' WHERE revision=1`,
		"history-actor":                     `UPDATE project_map_import_history SET actor_id='different-actor' WHERE revision=1`,
		"history-worker":                    `UPDATE project_map_import_history SET worker_id='different-worker' WHERE revision=1`,
		"history-source-key":                `UPDATE project_map_import_history SET record_json=json_set(record_json,'$.source_key','different-source') WHERE revision=1`,
		"history-document-inference":        `UPDATE project_map_import_history SET record_json=json_set(record_json,'$.document.entries[0].provenance.confidence','unrecognized') WHERE revision=1`,
		"history-document-map-disagreement": `UPDATE project_map_import_history SET record_json=json_set(record_json,'$.document.entries[0].entry.title','Not the captured map revision') WHERE revision=1`,
		"missing-history":                   `DELETE FROM project_map_import_history WHERE revision=1`,
		"omitted-binding-lost":              `DELETE FROM project_map_import_items WHERE item_key='track-b'`,
		"omitted-source-revision":           `UPDATE project_map_import_items SET source_revision=2 WHERE item_key='track-b'`,
		"mapping-entry-revision":            `UPDATE project_map_import_items SET entry_revision=2 WHERE item_key='track-b'`,
		"mapping-item-key":                  `UPDATE project_map_import_items SET item_key='unknown-item' WHERE item_key='track-b'`,
		"oversized-source-record":           `UPDATE project_map_import_sources SET record_json=json_set(record_json,'$.document.entries[0].provenance.uncertainty',replace(hex(zeroblob(300000)),'0','x'))`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			// Preserve agreement with the latest immutable pointer so these
			// cases exercise logical admission, not merely a row mismatch.
			if strings.HasPrefix(name, "current-") || name == "oversized-source-record" {
				change += `; UPDATE project_map_import_history SET record_json=(SELECT record_json FROM project_map_import_sources) WHERE revision=2`
			}
			mutateBundleSQL(t, bundle.Path, change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}

func TestMapImportBundleRejectsResealedSchemaDrift(t *testing.T) {
	f := mapImportRestoreFixture(t)
	for name, change := range map[string]string{
		"source-key-order":    `UPDATE sqlite_schema SET sql=replace(sql,'PRIMARY KEY(project_id,source_key)','PRIMARY KEY(source_key,project_id)') WHERE name='project_map_import_sources'`,
		"history-key-order":   `UPDATE sqlite_schema SET sql=replace(sql,'PRIMARY KEY(project_id,source_key,revision)','PRIMARY KEY(project_id,revision,source_key)') WHERE name='project_map_import_history'`,
		"mapping-key-order":   `UPDATE sqlite_schema SET sql=replace(sql,'PRIMARY KEY(project_id,source_key,item_key)','PRIMARY KEY(project_id,item_key,source_key)') WHERE name='project_map_import_items'`,
		"nullable-document":   `UPDATE sqlite_schema SET sql=replace(sql,'record_json TEXT NOT NULL','record_json TEXT') WHERE name='project_map_import_sources'`,
		"mapping-affinity":    `UPDATE sqlite_schema SET sql=replace(sql,'source_revision INTEGER NOT NULL','source_revision TEXT NOT NULL') WHERE name='project_map_import_items'`,
		"unknown-default":     `UPDATE sqlite_schema SET sql=replace(sql,'record_json TEXT NOT NULL','record_json TEXT NOT NULL DEFAULT ''{}''') WHERE name='project_map_import_history'`,
		"source-cascade":      `UPDATE sqlite_schema SET sql=replace(sql,'ON DELETE CASCADE','ON DELETE NO ACTION') WHERE name='project_map_import_sources'`,
		"mapping-grouped-key": `UPDATE sqlite_schema SET sql=replace(sql,'FOREIGN KEY(project_id,entry_id,entry_revision)','FOREIGN KEY(project_id,entry_revision,entry_id)') WHERE name='project_map_import_items'`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `PRAGMA writable_schema=ON;`+change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
