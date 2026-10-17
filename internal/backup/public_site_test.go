package backup

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func publicSiteRestoreFixture(t *testing.T, enabled bool) (*restoreFixture, *publicsite.Service) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	service := publicsite.New(f.store, func(context.Context, *sql.Tx, string, publicsite.Manifest, string) ([]publicsite.ReviewFacts, error) {
		return []publicsite.ReviewFacts{{Number: 9, Lifecycle: "merged", CLI: []publicsite.RoundFacts{{Key: "public_round_" + strings.Repeat("a", 32), Completed: true, Finalized: true, Complete: true, Found: 3, Accepted: 1, Routed: 1}}}}, nil
	})
	service.Register(f.handler.(*http.ServeMux), t.Context())
	m := publicsite.Manifest{Slug: "restore-showcase", Presentation: presentationmodel.Config{Name: "Approved public project"}, Description: "First approved public description", Reviews: []publicsite.EnrolledReview{{Number: 9, Title: "Approved merged history", URL: "https://github.com/synthetic/restore/pull/9"}}}
	publicSiteApply(t, f, m, 0, enabled)
	if enabled {
		m.Description = "Later approved public description"
		publicSiteApply(t, f, m, 1, true)
	}
	return f, service
}

func publicSiteApply(t *testing.T, f *restoreFixture, m publicsite.Manifest, revision int64, enabled bool) {
	t.Helper()
	m, err := publicsite.NormalizeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/projects/" + f.project + "/publication"
	input := map[string]any{"expected_revision": revision, "enabled": enabled, "manifest": m}
	preview := f.request(t, "POST", base+"/preview", input)
	input["preview_sha256"] = preview["preview_sha256"]
	f.request(t, "PUT", base, input)
}

func publicSiteAnonymous(t *testing.T, service *publicsite.Service, route string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	service.Register(mux, t.Context())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
	return w
}

func TestPublicSiteBundlePreservesApprovedHistoryAndInactiveBoundary(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled-after-publication", "never-enabled"} {
		t.Run(mode, func(t *testing.T) {
			f, service := publicSiteRestoreFixture(t, mode != "never-enabled")
			if mode == "disabled-after-publication" {
				f.request(t, "POST", "/api/v1/projects/"+f.project+"/publication/unpublish", map[string]any{"expected_revision": 2})
				m := publicsite.Manifest{Slug: "new-disabled-slug", Presentation: presentationmodel.Config{Name: "Approved disabled edit"}, Description: "Disabled approval is retained without publishing"}
				publicSiteApply(t, f, m, 3, false)
			}
			beforePublications := restoreRows(t, f.store.DB(), `SELECT * FROM project_publications ORDER BY project_id`)
			beforeSnapshots := restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
			beforeJobs := restoreRows(t, f.store.DB(), `SELECT * FROM jobs ORDER BY project_id,id`)
			beforeMessages := restoreRows(t, f.store.DB(), `SELECT * FROM inbox_messages ORDER BY sequence`)
			if mode == "enabled" {
				current, err := service.Current(t.Context(), "restore-showcase")
				if err != nil || current.Revision != 2 || current.Content.Reviews[0].Lifecycle != "merged" {
					t.Fatal("genuine public fixture unavailable", current, err)
				}
			}
			bundle := restoreBundle(t, f)
			beforeTree := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTree, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection modified immutable bundle")
			}
			target := filepath.Join(t.TempDir(), "restored")
			restored, err := Restore(t.Context(), bundle.Path, target)
			if err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			for name, pair := range map[string]struct {
				before [][]any
				query  string
			}{
				"publication":      {beforePublications, `SELECT * FROM project_publications ORDER BY project_id`},
				"snapshots":        {beforeSnapshots, `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`},
				"private-jobs":     {beforeJobs, `SELECT * FROM jobs ORDER BY project_id,id`},
				"private-messages": {beforeMessages, `SELECT * FROM inbox_messages ORDER BY sequence`},
			} {
				if !reflect.DeepEqual(pair.before, restoreRows(t, db, pair.query)) {
					t.Fatal("restore changed retained rows", name)
				}
			}
			var domain string
			if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:public_site'`).Scan(&domain); err != nil || domain != "1" {
				t.Fatal("public domain not preserved", domain, err)
			}
			db.Close()
			inactive, err := core.Open(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []string{"/api/v1/public/projects/restore-showcase", "/api/v1/public/projects/restore-showcase/history", "/api/v1/public/projects/restore-showcase/events?cursor=public_generation_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1"} {
				w := publicSiteAnonymous(t, publicsite.New(inactive, nil), route)
				if w.Code != 404 || strings.Contains(w.Body.String(), "Approved") {
					t.Fatal("inactive restore served anonymous content", route, w.Code, w.Body.String())
				}
			}
			if err = inactive.Close(); err != nil {
				t.Fatal(err)
			}
			tokenFile := filepath.Join(t.TempDir(), "fresh-admin.token")
			if err = os.WriteFile(tokenFile, []byte("synthetic-public-recovery-admin-token-123456789"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Activate(t.Context(), target, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: tokenFile}); err != nil {
				t.Fatal(err)
			}
			active := activationOpen(t, target)
			w := publicSiteAnonymous(t, publicsite.New(active, nil), "/api/v1/public/projects/restore-showcase")
			if mode == "enabled" {
				if w.Code != 200 || !strings.Contains(w.Body.String(), "Later approved public description") || strings.Contains(w.Body.String(), "Original brief") || strings.Contains(w.Body.String(), "Retained message") || strings.Contains(w.Body.String(), f.project) {
					t.Fatal("activated public projection wrong or leaked private records", w.Code, w.Body.String())
				}
				// A later explicit reenable creates its own public generation;
				// restoring/activating does not make a retired generation usable.
				oldGeneration := beforeSnapshots[0][1].(string)
				approved, err := publicsite.DecodeManifest([]byte(beforePublications[0][4].(string)))
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				reopened := publicsite.New(active, nil)
				reopened.Register(mux, t.Context())
				fresh := &restoreFixture{store: active, token: "synthetic-public-recovery-admin-token-123456789", project: f.project, worker: f.worker, handler: mux}
				fresh.request(t, "POST", "/api/v1/projects/"+f.project+"/publication/unpublish", map[string]any{"expected_revision": 2})
				publicSiteApply(t, fresh, approved, 3, true)
				current, err := reopened.Current(t.Context(), approved.Slug)
				if err != nil || current.Generation == oldGeneration {
					t.Fatal("reenable reused old publication generation", current, err)
				}
				retired := publicSiteAnonymous(t, reopened, "/api/v1/public/projects/"+approved.Slug+"/events?cursor="+oldGeneration+":1")
				if retired.Code != 409 {
					t.Fatal("retired generation resumed public delivery", retired.Code, retired.Body.String())
				}
			} else if w.Code != 404 {
				t.Fatal("activation silently published disabled settings", w.Code, w.Body.String())
			}
		})
	}
}

func TestPublicSiteBundleRejectsResealedContentCorruption(t *testing.T) {
	f, _ := publicSiteRestoreFixture(t, true)
	for name, query := range map[string]string{
		"unknown-public-field":      `UPDATE public_project_snapshots SET content_json=json_set(content_json,'$.private_actor','PRIVATE_SENTINEL') WHERE revision=1`,
		"private-round-key":         `UPDATE public_project_snapshots SET content_json=replace(content_json,'public_round_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','review_run_private') WHERE revision=1`,
		"missing-current-pointer":   `DELETE FROM public_project_snapshots WHERE revision=2`,
		"invalid-private-manifest":  `UPDATE project_publications SET manifest_json=json_set(manifest_json,'$.private_job_id','PRIVATE_SENTINEL')`,
		"manifest-slug-mismatch":    `UPDATE project_publications SET slug='other-public-slug'`,
		"invalid-salt":              `UPDATE project_publications SET round_salt='private-source-id'`,
		"invalid-generation":        `UPDATE project_publications SET generation='private-snapshot-id'`,
		"missing-current-time":      `UPDATE project_publications SET public_updated_at=''`,
		"unknown-refresh-state":     `UPDATE project_publications SET refresh_error='PRIVATE_DIAGNOSTIC'`,
		"changed-enabled-editorial": `UPDATE project_publications SET manifest_json=replace(manifest_json,'Later approved public description','Changed private draft description')`,
		"historical-bad-counts":     `UPDATE public_project_snapshots SET content_json=replace(content_json,'"accepted":1','"accepted":999') WHERE revision=1`,
		"new-old-generation":        `UPDATE public_project_snapshots SET generation='public_generation_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE revision=1`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}

func TestPublicSiteBundleRejectsResealedSchemaDrift(t *testing.T) {
	f, _ := publicSiteRestoreFixture(t, true)
	for name, change := range map[string]string{
		"snapshot-key-order": `UPDATE sqlite_schema SET sql=replace(sql,'PRIMARY KEY(project_id,generation,revision)','PRIMARY KEY(project_id,revision,generation)') WHERE name='public_project_snapshots'`,
		"nullable-salt":      `UPDATE sqlite_schema SET sql=replace(sql,'round_salt TEXT NOT NULL','round_salt TEXT') WHERE name='project_publications'`,
		"revision-affinity":  `UPDATE sqlite_schema SET sql=replace(sql,'revision INTEGER NOT NULL','revision TEXT NOT NULL') WHERE name='public_project_snapshots'`,
		"unknown-default":    `UPDATE sqlite_schema SET sql=replace(sql,'refresh_error TEXT NOT NULL','refresh_error TEXT NOT NULL DEFAULT ''private''') WHERE name='project_publications'`,
		"foreign-key-action": `UPDATE sqlite_schema SET sql=replace(sql,'ON DELETE CASCADE','ON DELETE NO ACTION') WHERE name='public_project_snapshots'`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `PRAGMA writable_schema=ON;`+change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
	// Rebuild just this disposable bundle table with otherwise supported columns
	// and keys, proving unique slug identity is checked independently of rows.
	t.Run("missing-unique-slug", func(t *testing.T) {
		bundle := restoreBundle(t, f)
		mutateBundleSQL(t, bundle.Path, `CREATE TEMP TABLE held_publications AS SELECT * FROM project_publications;
DROP TABLE project_publications;
CREATE TABLE project_publications(project_id TEXT PRIMARY KEY NOT NULL,revision INTEGER NOT NULL,slug TEXT NOT NULL,enabled INTEGER NOT NULL,manifest_json TEXT NOT NULL,round_salt TEXT NOT NULL,generation TEXT NOT NULL,public_revision INTEGER NOT NULL,public_updated_at TEXT NOT NULL,refresh_error TEXT NOT NULL,FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE);
INSERT INTO project_publications SELECT * FROM held_publications; DROP TABLE held_publications;`)
		_, err := Inspect(t.Context(), bundle.Path)
		backupCode(t, err, "backup_invalid")
	})
}
