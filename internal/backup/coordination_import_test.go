package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/jobs"
)

func coordinationImportRestoreFixture(t *testing.T) (*restoreFixture, coordimport.Batch, string) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := coordimport.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	coordimport.Register(f.handler.(*http.ServeMux), f.store)
	revision := int64(0)
	batch := coordimport.Batch{Version: 1, RepositoryID: 991, ExpectedRevision: &revision,
		Source: "selected-coordination-export", SourceSHA256: strings.Repeat("a", 64), Jobs: []jobs.PreparedJob{{
			SourceKey: "source-job", SourceRevision: 37, Title: "Imported original", Brief: "Original historical brief <literal>",
			WorkerID: f.worker, Status: "parked", CreatedAt: "2025-01-01T01:02:03Z", UpdatedAt: "2025-02-01T04:05:06Z",
			Checkpoints: []jobs.PreparedCheckpoint{
				{SourceKey: "first-checkpoint", ActorID: "historical-actor", WorkerID: f.worker, Done: "Original done", Next: "Original next", Blocker: "Original blocker", CreatedAt: "2025-01-15T01:02:03Z", Evidence: []string{"https://example.com/original-proof"}},
				{SourceKey: "second-checkpoint", ActorID: "another-historical-actor", Done: "Later done", Next: "Later next", CreatedAt: "2025-02-01T04:05:06Z", Evidence: []string{}},
			},
		}}}
	path := "/api/v1/projects/" + f.project + "/coordination/imports"
	preview := f.request(t, "POST", path+"/preview", batch)
	applied := f.request(t, "POST", path+"/apply", coordimport.ApplyRequest{Batch: batch, PreviewSHA256: preview["preview_sha256"].(string)})
	body, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	var receipt coordimport.Receipt
	if json.Unmarshal(body, &receipt) != nil || receipt.Revision != 1 || receipt.Outcome != "imported" || receipt.Counts.NewJobs != 1 || receipt.Counts.Checkpoints != 2 || len(receipt.Mappings) != 1 {
		t.Fatal("authenticated import did not produce expected native state")
	}
	id := receipt.Mappings[0].JobID
	// Normal adoption edits must not invalidate immutable import provenance.
	f.request(t, "PATCH", "/api/v1/projects/"+f.project+"/jobs/"+id, map[string]any{
		"expected_revision": 1, "title": "Current adopted title", "brief": "Current native brief", "status": "blocked",
	})
	return f, batch, id
}

func TestCoordinationImportBackupPreservesOriginalAndEvolvedState(t *testing.T) {
	f, batch, id := coordinationImportRestoreFixture(t)
	beforeRows := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	beforeFiles := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	receipt, err := Restore(t.Context(), bundle.Path, target)
	if err != nil || receipt.State != "inactive" {
		t.Fatal("inactive restore failed", err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	afterRows := backupRows(t, db)
	for _, name := range []string{"coordination_import_state", "coordination_import_records", "coordination_import_batches", "jobs", "job_history", "job_checkpoints", "inbox_messages", "receipts", "events", "review_operations"} {
		if beforeRows[name] == "" || beforeRows[name] != afterRows[name] {
			t.Fatalf("restored %s lost retained state", name)
		}
	}
	var source, key, sourceHash, itemJSON string
	var importRevision int64
	if err = db.QueryRow(`SELECT source,source_key,source_sha256,item_json,import_revision FROM coordination_import_records WHERE project_id=? AND job_id=?`, f.project, id).Scan(&source, &key, &sourceHash, &itemJSON, &importRevision); err != nil {
		t.Fatal(err)
	}
	var original jobs.PreparedJob
	if json.Unmarshal([]byte(itemJSON), &original) != nil || !reflect.DeepEqual(original, batch.Jobs[0]) || source != batch.Source || sourceHash != batch.SourceSHA256 || key != original.SourceKey || importRevision != 1 {
		t.Fatal("source provenance or selected checkpoint history changed")
	}
	var title, brief, status string
	var revision int64
	if err = db.QueryRow(`SELECT title,brief,status,revision FROM jobs WHERE project_id=? AND id=?`, f.project, id).Scan(&title, &brief, &status, &revision); err != nil || title != "Current adopted title" || brief != "Current native brief" || status != "blocked" || revision != 2 {
		t.Fatal("ordinary later native edit was lost", err)
	}
	var originalActor, originalJSON string
	if err = db.QueryRow(`SELECT actor_id,state_json FROM job_history WHERE project_id=? AND job_id=? AND revision=1`, f.project, id).Scan(&originalActor, &originalJSON); err != nil || originalActor != "imported-history" {
		t.Fatal("native import origin lost", err)
	}
	var history struct {
		Title    string `json:"title"`
		Brief    string `json:"brief"`
		Revision int64  `json:"revision"`
	}
	if json.Unmarshal([]byte(originalJSON), &history) != nil || history.Title != original.Title || history.Brief != original.Brief || history.Revision != 1 {
		t.Fatal("source revision was substituted for native revision one")
	}
	if !reflect.DeepEqual(beforeFiles, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(beforeRows, backupRows(t, f.store.DB())) {
		t.Fatal("inspection or restore changed original bundle or source")
	}
}

func TestCoordinationImportBackupRejectsResealedLogicalCorruption(t *testing.T) {
	for _, kind := range []string{"source_hash", "item_json", "identity", "batch_revision", "original_history", "checkpoint", "blob_source"} {
		t.Run(kind, func(t *testing.T) {
			f, batch, id := coordinationImportRestoreFixture(t)
			if kind == "blob_source" {
				// Reach the provenance keyset page boundary through genuine imports,
				// not SQL-manufactured mappings. BLOB keys formerly repeated page one.
				revision := int64(1)
				batch.ExpectedRevision = &revision
				original := batch.Jobs[0]
				batch.Jobs = make([]jobs.PreparedJob, 24)
				for i := range batch.Jobs {
					batch.Jobs[i] = original
					batch.Jobs[i].SourceKey = "additional-job-" + strconv.Itoa(i)
				}
				path := "/api/v1/projects/" + f.project + "/coordination/imports"
				preview := f.request(t, "POST", path+"/preview", batch)
				f.request(t, "POST", path+"/apply", coordimport.ApplyRequest{Batch: batch, PreviewSHA256: preview["preview_sha256"].(string)})
				var count int
				if err := f.store.DB().QueryRow(`SELECT count(*) FROM coordination_import_records`).Scan(&count); err != nil || count != 25 {
					t.Fatal("actual imports did not fill one provenance page", err)
				}
			}
			sourceRows := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			switch kind {
			case "source_hash":
				mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_records SET source_sha256=?`, strings.Repeat("b", 64))
			case "blob_source":
				mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_records SET source=CAST(source AS BLOB)`)
			case "item_json":
				// Rehash the item as well as the bundle, proving semantic validation
				// still refuses a hash-consistent but incomplete prepared record.
				sum := sha256.Sum256([]byte(`{}`))
				mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_records SET item_json='{}',item_sha256=?`, hex.EncodeToString(sum[:]))
			case "identity":
				mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_records SET source_key='different-source-key'`)
			case "batch_revision":
				mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_state SET revision=2`)
			case "original_history":
				mutateBundleSQL(t, bundle.Path, `UPDATE job_history SET state_json=json_set(state_json,'$.brief','Changed historical original') WHERE job_id=? AND revision=1`, id)
			case "checkpoint":
				mutateBundleSQL(t, bundle.Path, `UPDATE job_checkpoints SET done='Changed historical checkpoint' WHERE job_id=?`, id)
			}
			before := restoreTree(t, bundle.Path)
			ctx := t.Context()
			if kind == "blob_source" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
			}
			_, err := Inspect(ctx, bundle.Path)
			// A canceled/timed-out keyset loop is not a successful refusal.
			backupCode(t, err, "backup_invalid")
			parent := t.TempDir()
			sentinel := filepath.Join(parent, "unrelated")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(parent, "must-not-exist")
			_, err = Restore(ctx, bundle.Path, target)
			backupCode(t, err, "backup_invalid")
			if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("logical corruption created a restore destination", err)
			}
			if body, err := os.ReadFile(sentinel); err != nil || string(body) != "preserve" {
				t.Fatal("refusal changed unrelated sibling", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(sourceRows, backupRows(t, f.store.DB())) {
				t.Fatal("refusal modified bundle or live fixture source")
			}
		})
	}
}

func TestCoordinationImportBackupAcceptsOlderBundleWithoutDomain(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM metadata WHERE key='schema:coordination_import'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("older bundle acquired an uninstalled optional domain", err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'coordination_import_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("inspection or restore initialized the optional schema", err)
	}
}

func TestCoordinationImportBackupRequiresPhysicalMappingKeys(t *testing.T) {
	for _, kind := range []string{"missing_job_uniqueness", "changed_primary_order", "casefolded_job_uniqueness", "equivalent_explicit_unique"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := coordinationImportRestoreFixture(t)
			var ddl string
			if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='coordination_import_records'`).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM coordination_import_records`).Scan(&count); err != nil || count != 1 {
				t.Fatal("schema proof must not rely on conflicting data", err)
			}
			change, replacement := "", ""
			switch kind {
			case "missing_job_uniqueness", "equivalent_explicit_unique":
				change = "UNIQUE(project_id,job_id),"
			case "changed_primary_order":
				change, replacement = "PRIMARY KEY(project_id,source,source_key)", "PRIMARY KEY(project_id,source_key,source)"
			case "casefolded_job_uniqueness":
				change, replacement = "UNIQUE(project_id,job_id)", "UNIQUE(project_id,job_id COLLATE NOCASE)"
			}
			if strings.Count(ddl, change) != 1 {
				t.Fatal("canonical fixture DDL changed; update the targeted damage")
			}
			changed := strings.Replace(ddl, change, replacement, 1)
			bundle := restoreBundle(t, f)
			query := `PRAGMA foreign_keys=OFF; ALTER TABLE coordination_import_records RENAME TO previous_import_records; ` + changed + `;
				INSERT INTO coordination_import_records SELECT * FROM previous_import_records;
				DROP TABLE previous_import_records;`
			if kind == "equivalent_explicit_unique" {
				query += `CREATE UNIQUE INDEX explicit_import_job_binding ON coordination_import_records(project_id,job_id);`
			}
			mutateBundleSQL(t, bundle.Path, query)
			before := restoreTree(t, bundle.Path)
			target := filepath.Join(t.TempDir(), "restored")
			_, inspectErr := Inspect(t.Context(), bundle.Path)
			_, restoreErr := Restore(t.Context(), bundle.Path, target)
			if kind == "equivalent_explicit_unique" {
				if inspectErr != nil || restoreErr != nil {
					t.Fatal("equivalent nonpartial BINARY ascending uniqueness refused", inspectErr, restoreErr)
				}
			} else {
				backupCode(t, inspectErr, "backup_invalid")
				backupCode(t, restoreErr, "backup_invalid")
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing physical guarantee created restore destination", err)
				}
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("physical validation changed the sealed bundle")
			}
		})
	}
}
