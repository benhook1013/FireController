package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func publicWindowBackupRounds(start, count int) []publicsite.RoundFacts {
	rounds := make([]publicsite.RoundFacts, count)
	for i := range rounds {
		rounds[i] = publicsite.RoundFacts{Key: fmt.Sprintf("public_round_%032x", start+i), Completed: true, Finalized: true, Complete: true, Found: 3, Accepted: 1, Routed: 1, ObservedAt: "2026-10-17T01:00:00Z"}
	}
	return rounds
}

func publicWindowInventoryFixture(t *testing.T) (*restoreFixture, *publicsite.Service, func(), string) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	cli := publicWindowBackupRounds(1, 100)
	hosted := []publicsite.RoundFacts{}
	// Controlled retained source inventories have more eligible rounds after
	// growth. The service receives the same bounded100+more shape as the adapter.
	reader := func(context.Context, *sql.Tx, string, publicsite.Manifest, string) ([]publicsite.ReviewFacts, error) {
		c, h := cli, hosted
		cm, hm := len(c) > 100, len(h) > 100
		if cm {
			c = c[len(c)-100:]
		}
		if hm {
			h = h[len(h)-100:]
		}
		return []publicsite.ReviewFacts{{Number: 9, Lifecycle: "merged", CLI: c, Hosted: h, CLIHasMore: cm, HostedHasMore: hm}}, nil
	}
	service := publicsite.New(f.store, reader)
	service.Register(f.handler.(*http.ServeMux), t.Context())
	manifest := publicsite.Manifest{Slug: "window-backup", Presentation: presentationmodel.Config{Name: "Approved window project"}, Description: "Approved retained project", Reviews: []publicsite.EnrolledReview{{Number: 9, Title: "Approved retained PR", URL: "https://github.com/synthetic/window/pull/9"}}}
	publicSiteApply(t, f, manifest, 0, true)
	legacyContent, err := publicsite.BuildContent(manifest, []publicsite.ReviewFacts{{Number: 9, Lifecycle: "merged", CLI: cli, Hosted: hosted}})
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(legacyContent)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND revision=1`, f.project).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(legacyJSON) || strings.Contains(stored, "has_more") {
		t.Fatal("initial approved publication changed genuine legacy canonical JSON")
	}
	grow := func() {
		cli = publicWindowBackupRounds(101, 101)
		hosted = publicWindowBackupRounds(1001, 101)
		if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return publicsite.Refresh(ctx, tx, f.project, reader) }); err != nil {
			t.Fatal(err)
		}
		current, err := service.Current(t.Context(), manifest.Slug)
		if err != nil || current.Revision != 2 || !current.Content.Reviews[0].CLIHasMore || !current.Content.Reviews[0].HostedHasMore || len(current.Content.Reviews[0].CLI) != 100 || len(current.Content.Reviews[0].Hosted) != 100 {
			t.Fatal("actual refresh did not publish bounded channel windows", err)
		}
		if current.Content.Reviews[0].CLI[0].Key != cli[1].Key || current.Content.Reviews[0].Hosted[0].Key != hosted[1].Key {
			t.Fatal("service did not preserve selected newest safe summaries")
		}
		var old string
		if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND revision=1`, f.project).Scan(&old); err != nil || old != stored {
			t.Fatal("window refresh rewrote legacy snapshot", err)
		}
	}
	return f, service, grow, stored
}

func publicWindowInspectInactiveRestore(t *testing.T, f *restoreFixture, bundle Summary, want [][]any) {
	t.Helper()
	tree := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("canonical public window bundle rejected", err)
	}
	if !reflect.DeepEqual(tree, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection rewrote immutable bundle")
	}
	target := filepath.Join(t.TempDir(), "restored-window")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	actual := restoreRows(t, db, `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:public_site'`).Scan(&version); err != nil || version != "1" {
		db.Close()
		t.Fatal("window changed physical public schema", err)
	}
	db.Close()
	if !reflect.DeepEqual(actual, want) {
		t.Fatal("inactive restore changed frozen public snapshot bytes")
	}
	inactive, err := core.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer inactive.Close()
	response := publicSiteAnonymous(t, publicsite.New(inactive, nil), "/api/v1/public/projects/window-backup")
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "Approved") {
		t.Fatal("inactive restore exposed public window")
	}
	if !reflect.DeepEqual(want, restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)) {
		t.Fatal("bundle/restore changed source public snapshots")
	}
}

func TestPublicWindowBundlePreservesLegacyAndWindowedSnapshots(t *testing.T) {
	f, service, grow, legacy := publicWindowInventoryFixture(t)
	first := restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
	publicWindowInspectInactiveRestore(t, f, restoreBundle(t, f), first)
	grow()
	current, err := service.Current(t.Context(), "window-backup")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(current.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{f.project, f.worker, f.principal, f.token, "review_run_private_source", "private_source_attempt"} {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatal("public window flags exposed private source identity")
		}
	}
	snapshots := restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
	if len(snapshots) != 2 || snapshots[0][3].(string) != legacy {
		t.Fatal("legacy bytes were not retained next to the new window")
	}
	publicWindowInspectInactiveRestore(t, f, restoreBundle(t, f), snapshots)
}

func TestPublicWindowBundleRejectsResealedMalformedFlagsBeforeRestore(t *testing.T) {
	f, _, grow, _ := publicWindowInventoryFixture(t)
	grow()
	baseline := restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)
	for name, query := range map[string]string{
		"boolean-to-number":          `UPDATE public_project_snapshots SET content_json=replace(content_json,'"cli_has_more":true','"cli_has_more":1') WHERE revision=2`,
		"null-flag":                  `UPDATE public_project_snapshots SET content_json=replace(content_json,'"cli_has_more":true','"cli_has_more":null') WHERE revision=2`,
		"string-flag":                `UPDATE public_project_snapshots SET content_json=replace(content_json,'"hosted_has_more":true','"hosted_has_more":"true"') WHERE revision=2`,
		"unknown-field":              `UPDATE public_project_snapshots SET content_json=replace(content_json,'"cli_has_more":true','"private_source_attempt":"PRIVATE_SENTINEL"') WHERE revision=2`,
		"noncanonical-present-false": `UPDATE public_project_snapshots SET content_json=replace(content_json,'"cli_has_more":true','"cli_has_more":false') WHERE revision=2`,
		"historical-present-false":   `UPDATE public_project_snapshots SET content_json=json_set(content_json,'$.reviews[0].cli_has_more',json('false')) WHERE revision=1`,
		"historical-null-flag":       `UPDATE public_project_snapshots SET content_json=json_set(content_json,'$.reviews[0].hosted_has_more',NULL) WHERE revision=1`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			tree := restoreTree(t, bundle.Path)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			target := filepath.Join(t.TempDir(), "rejected-window-restore")
			_, err = Restore(t.Context(), bundle.Path, target)
			backupCode(t, err, "backup_invalid")
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("malformed public flags reached destination writes", err)
			}
			if !reflect.DeepEqual(tree, restoreTree(t, bundle.Path)) {
				t.Fatal("rejected inspection/restore changed resealed input")
			}
			if !reflect.DeepEqual(baseline, restoreRows(t, f.store.DB(), `SELECT * FROM public_project_snapshots ORDER BY project_id,generation,revision`)) {
				t.Fatal("negative fixture changed authoritative source")
			}
		})
	}
}
