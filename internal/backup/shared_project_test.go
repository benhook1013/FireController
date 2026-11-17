package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/server"
)

// All accepted data comes through the canonical project/job/summary handlers.
func seedBackupJobSummaries(t *testing.T, f *restoreFixture) string {
	t.Helper()
	job := restoreRows(t, f.store.DB(), `SELECT id FROM jobs LIMIT 1`)[0][0].(string)
	base := "/api/v1/projects/" + f.project + "/jobs/" + job
	f.request(t, "PUT", base+"/summary", map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "First concise title", "body": "Original selected summary <literal> 雪\n\tline"})
	f.request(t, "PATCH", base, map[string]any{"expected_revision": 1, "brief": "Private standing instructions change without rewriting summary history"})
	f.request(t, "PUT", base+"/summary", map[string]any{"expected_job_revision": 2, "expected_revision": 1, "title": "Independent second summary", "body": "Separate authored body"})
	f.request(t, "PUT", base+"/summary", map[string]any{"expected_job_revision": 2, "expected_revision": 2, "title": "", "body": ""})
	f.request(t, "PUT", base+"/summary", map[string]any{"expected_job_revision": 2, "expected_revision": 3, "title": strings.Repeat("<", 240), "body": strings.Repeat("<", 1990) + "雪\r\n\tend"})
	return job
}

func TestSharedProjectBackupRetainsHandlesAndExactSummaryHistory(t *testing.T) {
	f := newRestoreFixture(t)
	seedBackupJobSummaries(t, f)
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
		t.Fatal(restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatal("retained table changed", table)
		}
	}
	if got := restoreRows(t, db, `SELECT count(*) FROM credentials WHERE revoked=0`)[0][0].(int64); got != 0 {
		t.Fatal("restored access revived")
	}
	if len(restoreRows(t, db, `SELECT * FROM job_summary_history`)) != 4 || len(restoreRows(t, db, `SELECT * FROM project_handles`)) != 1 {
		t.Fatal("new records missing")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("source or immutable bundle changed")
	}
}

func TestSharedProjectBackupAcceptsPriorJobsSixAndAbsentHandlesWithoutMigration(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := f.store.DB().Exec(`DROP TABLE job_progress_history; DROP TABLE job_progress; DROP TABLE job_summary_history;DROP TABLE job_summaries;UPDATE metadata SET value='6' WHERE key='schema:jobs';DROP TABLE project_handles;DELETE FROM metadata WHERE key='schema:project_handles'`); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "old-inactive")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	if got := generationRead(t, target, `SELECT value FROM metadata WHERE key='schema:jobs'`)[0][0]; got != "6" {
		t.Fatal("predecessor schema changed", got)
	}
	if rows := generationRead(t, target, `SELECT name FROM sqlite_schema WHERE name IN ('project_handles','job_summaries','job_summary_history')`); len(rows) != 0 {
		t.Fatal("inspection or restore migrated predecessor")
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("predecessor bundle changed")
	}
}

func assertSharedProjectBundleRefused(t *testing.T, path string) {
	t.Helper()
	before := restoreTree(t, path)
	_, err := Inspect(t.Context(), path)
	backupCode(t, err, "backup_invalid")
	destination := filepath.Join(t.TempDir(), "refused")
	_, err = Restore(t.Context(), path, destination)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid bundle created restore destination", err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, path)) {
		t.Fatal("refused bundle changed")
	}
}

func TestSharedProjectBackupRefusesResealedSemanticAndPhysicalCorruption(t *testing.T) {
	f := newRestoreFixture(t)
	seedBackupJobSummaries(t, f)
	before := backupRows(t, f.store.DB())
	cases := []struct{ name, sql string }{
		{"missing-handle", `DELETE FROM project_handles`},
		{"handle-blob", `PRAGMA ignore_check_constraints=ON;UPDATE project_handles SET handle=CAST(handle AS BLOB);PRAGMA ignore_check_constraints=OFF`},
		{"missing-summary-middle", `DELETE FROM job_summary_history WHERE revision=2`},
		{"missing-summary-latest", `DELETE FROM job_summary_history WHERE revision=4`},
		{"missing-summary-current", `DELETE FROM job_summaries`},
		{"divergent-current", `UPDATE job_summaries SET body='Different current prose'`},
		{"unretained-source-job-revision", `UPDATE job_summaries SET job_revision=99;UPDATE job_summary_history SET job_revision=99 WHERE revision=4`},
		{"invalid-historical-actor", `UPDATE job_summary_history SET actor_id='../other' WHERE revision=1`},
		{"oversized-escaped-original", `UPDATE job_summary_history SET body=replace(hex(zeroblob(1001)),'0','<') WHERE revision=1`},
		{"historical-body-blob", `UPDATE job_summary_history SET body=CAST(body AS BLOB) WHERE revision=1`},
		{"unexpected-summary-index", `CREATE INDEX unexpected_summary ON job_summary_history(body)`},
		{"changed-handle-check", `PRAGMA writable_schema=ON;UPDATE sqlite_schema SET sql=replace(sql,'BETWEEN 1 AND 63','BETWEEN 1 AND 64') WHERE name='project_handles';PRAGMA writable_schema=OFF`},
		{"changed-summary-type", `PRAGMA writable_schema=ON;UPDATE sqlite_schema SET sql=replace(sql,'revision INTEGER NOT NULL CHECK (revision >= 1)','revision REAL NOT NULL CHECK (revision >= 1)') WHERE name='job_summaries';PRAGMA writable_schema=OFF`},
		{"changed-summary-foreign-action", `PRAGMA writable_schema=ON;UPDATE sqlite_schema SET sql=replace(sql,'ON DELETE CASCADE','ON DELETE RESTRICT') WHERE name='job_summary_history';PRAGMA writable_schema=OFF`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.sql)
			assertSharedProjectBundleRefused(t, bundle.Path)
		})
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("corruption tests changed canonical source")
	}
}

func TestSharedProjectBackupRefusesOtherProjectsPublicationSlug(t *testing.T) {
	f, _ := publicSiteRestoreFixture(t, true)
	originalHandler, originalWorker := f.handler, f.worker
	f.worker = ""
	f.handler = server.New(f.store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 992, FullName: "synthetic/other"}, nil
	}))
	second := f.request(t, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Second project", "handle": "second-project", "repository": "synthetic/other"})["id"].(string)
	f.handler, f.worker = originalHandler, originalWorker
	// The public manifest and retained public snapshots remain valid; only the
	// other project's handle is resealed into an ambiguous publication address.
	bundle := restoreBundle(t, f)
	mutateBundleSQL(t, bundle.Path, `UPDATE project_handles SET handle='restore-showcase' WHERE project_id=?`, second)
	assertSharedProjectBundleRefused(t, bundle.Path)
}

// Retained linked sources are authored through their native HTTP handlers. No
// source tables or publication snapshots are fabricated for the valid fixture.
func linkedSharedProjectBackupFixture(t *testing.T) (*restoreFixture, *publicsite.Service, string, *atomic.Bool) {
	t.Helper()
	f := newRestoreFixture(t)
	for _, ensure := range []func(context.Context, *core.Store) error{projectmap.EnsureSchema, publicsite.EnsureSchema} {
		if err := ensure(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	failure := &atomic.Bool{}
	reader := func(context.Context, *sql.Tx, string, publicsite.Manifest, string) ([]publicsite.ReviewFacts, error) {
		if failure.Load() {
			return nil, errors.New("synthetic optional projection failure")
		}
		return []publicsite.ReviewFacts{}, nil
	}
	// Rebuild only the fixture mux to install the canonical transactional job
	// publication hook; the already retained store and all records stay intact.
	mux := server.New(f.store)
	jobs.Register(mux, f.store, jobs.WithPublicProjection(func(ctx context.Context, tx *sql.Tx, project, job string) error {
		return publicsite.RefreshForJob(ctx, tx, project, job, reader)
	}))
	projectmap.Register(mux, f.store)
	service := publicsite.New(f.store, reader)
	service.Register(mux, t.Context())
	f.handler = mux
	job := seedBackupJobSummaries(t, f)
	entry := projectmap.Editable{Kind: "workstream", Title: "Approved original map title", Description: "Approved original map narrative", State: "in progress", Where: "PRIVATE_WHERE", Next: "PRIVATE_NEXT", Phases: []projectmap.Phase{}, Links: []projectmap.Link{{Label: "PRIVATE_MAP_LINK", URL: "https://private.invalid"}}}
	base := "/api/v1/projects/" + f.project
	created := f.request(t, "POST", base+"/map/entries", entry)
	mapID := created["id"].(string)
	entry.Title, entry.Description, entry.Archived = "Later private map title", "Later unselected map narrative", true
	f.request(t, "PUT", base+"/map/entries/"+mapID, map[string]any{"expected_revision": 1, "entry": entry})
	manifest := publicsite.Manifest{
		Slug: "linked-backup", Presentation: presentationmodel.Config{Name: "Linked public project"}, Description: "Explicitly approved linked content",
		JobRefs:    []publicsite.JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}},
		MapRefs:    []publicsite.MapRef{{Key: "selected-map", EntryID: mapID, Revision: 1}},
		WorkerRefs: []publicsite.WorkerRef{{Key: "selected-worker", WorkerID: f.worker, Label: "Approved worker label"}},
	}
	publicSiteApply(t, f, manifest, 0, true)
	current, err := service.Current(t.Context(), manifest.Slug)
	if err != nil || len(current.Content.Progress) != 1 || current.Content.Progress[0].Title != "First concise title" || current.Content.Progress[0].Description != "Original selected summary <literal> 雪\n\tline" || current.Content.Progress[0].Status != "active" || len(current.Content.ProjectMap) != 1 || current.Content.ProjectMap[0].Title != "Approved original map title" || len(current.Content.WorkerLanes) != 1 || current.Content.WorkerLanes[0].Headline != "First concise title" {
		t.Fatal("canonical linked publication did not use exact selected history", current, err)
	}
	return f, service, job, failure
}

func TestSharedProjectLinkedPublicationBackupPreservesExactRefsAndPublicBytes(t *testing.T) {
	for _, mode := range []string{"current", "last-good-after-assignment-status-change"} {
		t.Run(mode, func(t *testing.T) {
			f, service, job, failure := linkedSharedProjectBackupFixture(t)
			publicBefore := publicSiteAnonymous(t, service, "/api/v1/public/projects/linked-backup")
			if publicBefore.Code != http.StatusOK {
				t.Fatal(publicBefore.Code, publicBefore.Body.String())
			}
			for _, private := range []string{f.project, f.worker, job, "job_refs", "map_refs", "worker_refs", "PRIVATE_WHERE", "PRIVATE_NEXT", "PRIVATE_MAP_LINK", "Later private map title", "Separate authored body", "Private standing instructions"} {
				if strings.Contains(publicBefore.Body.String(), private) {
					t.Fatalf("public linked content leaked unselected field %q", private)
				}
			}
			if mode != "current" {
				failure.Store(true)
				base := "/api/v1/projects/" + f.project
				f.request(t, "PATCH", base+"/jobs/"+job, map[string]any{"expected_revision": 2, "status": "blocked", "worker_id": ""})
				configuration := f.request(t, "GET", base+"/publication", nil)
				if configuration["refresh_error"] != "projection_unavailable" {
					t.Fatal("optional failure was not retained", configuration)
				}
				if current := f.request(t, "GET", base+"/jobs/"+job, nil); current["status"] != "blocked" || current["worker_id"] != "" {
					t.Fatal("optional publication failure discarded authoritative change", current)
				}
				lastGood := publicSiteAnonymous(t, service, "/api/v1/public/projects/linked-backup")
				if lastGood.Code != http.StatusOK || lastGood.Body.String() != publicBefore.Body.String() {
					t.Fatal("failed projection rewrote last-good bytes")
				}
			}
			before := backupRows(t, f.store.DB())
			publications := restoreRows(t, f.store.DB(), `SELECT * FROM project_publications ORDER BY project_id`)
			snapshots := restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
			var retained publicsite.Manifest
			if err := json.Unmarshal([]byte(publications[0][4].(string)), &retained); err != nil || len(retained.JobRefs) != 1 || retained.JobRefs[0].JobID != job || retained.JobRefs[0].SummaryRevision != 1 || len(retained.MapRefs) != 1 || retained.MapRefs[0].Revision != 1 || len(retained.WorkerRefs) != 1 || retained.WorkerRefs[0].WorkerID != f.worker {
				t.Fatal("private exact source references unavailable", retained, err)
			}
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("valid linked publication bundle refused", err)
			}
			destination := filepath.Join(t.TempDir(), "linked-inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
				t.Fatal(restored, err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			after := backupRows(t, db)
			for table, hash := range before {
				if table != "metadata" && table != "credentials" && after[table] != hash {
					t.Fatal("linked inactive restore changed retained table", table)
				}
			}
			if !reflect.DeepEqual(publications, restoreRows(t, db, `SELECT * FROM project_publications ORDER BY project_id`)) || !reflect.DeepEqual(snapshots, restoreRows(t, db, `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)) {
				t.Fatal("exact private manifest or public snapshot bytes changed")
			}
			if got := restoreRows(t, db, `SELECT count(*) FROM credentials WHERE revoked=0`)[0][0].(int64); got != 0 {
				t.Fatal("inactive restore revived access")
			}
			inactive, err := core.Open(t.Context(), destination)
			if err != nil {
				t.Fatal(err)
			}
			if response := publicSiteAnonymous(t, publicsite.New(inactive, nil), "/api/v1/public/projects/linked-backup"); response.Code != http.StatusNotFound {
				t.Fatal("inactive restore served retained public content", response.Code)
			}
			if err := inactive.Close(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("read-only backup qualification changed source or bundle")
			}
		})
	}
}

func TestSharedProjectLinkedPublicationBackupRefusesResealedPinnedSourceDamage(t *testing.T) {
	f, _, _, _ := linkedSharedProjectBackupFixture(t)
	before := backupRows(t, f.store.DB())
	var raw string
	if err := f.store.DB().QueryRow(`SELECT manifest_json FROM project_publications WHERE project_id=?`, f.project).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []int64{3, 99} {
		t.Run("unavailable-pinned-revision-"+strconv.FormatInt(revision, 10), func(t *testing.T) {
			manifest, err := publicsite.DecodeManifest([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			manifest.JobRefs[0].SummaryRevision = revision
			changed, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `UPDATE project_publications SET manifest_json=?`, string(changed))
			assertSharedProjectBundleRefused(t, bundle.Path)
		})
	}
	t.Run("changed-retained-summary-prose", func(t *testing.T) {
		bundle := restoreBundle(t, f)
		// This remains valid bounded summary history, with the same current
		// revision4. Its selected revision1 can no longer justify published prose.
		mutateBundleSQL(t, bundle.Path, `UPDATE job_summary_history SET body='Different welltyped selected summary body' WHERE revision=1`)
		assertSharedProjectBundleRefused(t, bundle.Path)
	})
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("resealed corruption proof changed canonical source")
	}
}
