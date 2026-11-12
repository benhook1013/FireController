package publicsite

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func publicationScope(t *testing.T, f *publicationFixture, token string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", f.base()+"/publication", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", f.worker)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func requirePublicationForbidden(t *testing.T, err error) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" {
		t.Errorf("want forbidden, got %v", err)
	}
}

func TestPublicationRejectsProjectManager(t *testing.T) {
	f := newPublicationFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	scope := publicationScope(t, f, f.reader)
	input := Approved{0, true, testPublicManifest()}
	preview := f.service.previewHash(scope, input)
	base := f.base() + "/publication"
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"GET", base, nil},
		{"POST", base + "/preview", input},
		{"PUT", base, map[string]any{"expected_revision": 0, "enabled": true, "manifest": input.Manifest, "preview_sha256": preview}},
		{"POST", base + "/unpublish", map[string]any{"expected_revision": 1}},
	} {
		t.Run(call.method+call.path, func(t *testing.T) {
			w := f.call(t, call.method, call.path, f.reader, "manager-publication", call.body)
			if w.Code != 403 {
				t.Fatalf("manager publication returned %d: %s", w.Code, w.Body.String())
			}
		})
	}
	// Service callers must receive the same boundary, even with a valid project
	// administrative scope constructed outside the HTTP publication handlers.
	_, err := f.service.Configuration(t.Context(), scope)
	requirePublicationForbidden(t, err)
	_, err = f.service.Preview(t.Context(), scope, input)
	requirePublicationForbidden(t, err)
	_, err = f.service.Apply(t.Context(), scope, "manager-apply", input, preview)
	requirePublicationForbidden(t, err)
	_, err = f.service.Unpublish(t.Context(), scope, "manager-unpublish", 0)
	requirePublicationForbidden(t, err)
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM project_publications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("manager changed publication", count, err)
	}
}

func TestPublicationRechecksCurrentAdministratorBeforeReceiptReplay(t *testing.T) {
	for _, mode := range []string{"demoted", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newPublicationFixture(t)
			scope := publicationScope(t, f, f.admin)
			input := Approved{0, true, testPublicManifest()}
			preview, err := f.service.Preview(t.Context(), scope, input)
			if err != nil {
				t.Fatal(err)
			}
			applied, err := f.service.Apply(t.Context(), scope, "admin-apply", input, preview.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := f.service.Apply(t.Context(), scope, "admin-apply", input, preview.SHA256)
			if err != nil || !bytes.Equal(applied, replay) {
				t.Fatal("admin apply replay", err)
			}
			unpublished, err := f.service.Unpublish(t.Context(), scope, "admin-unpublish", 1)
			if err != nil {
				t.Fatal(err)
			}
			replay, err = f.service.Unpublish(t.Context(), scope, "admin-unpublish", 1)
			if err != nil || !bytes.Equal(unpublished, replay) {
				t.Fatal("admin unpublish replay", err)
			}
			// Demotion retains project management, so the ordinary AdminScope contract
			// still permits it. Publication must not leak either retained receipt.
			digest := sha256.Sum256([]byte(f.admin))
			hash := hex.EncodeToString(digest[:])
			if _, err = f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) SELECT id,?,1,1,1 FROM credentials WHERE token_sha256=?`, f.project, hash); err != nil {
				t.Fatal(err)
			}
			query := `UPDATE credentials SET is_admin=0 WHERE token_sha256=?`
			if mode == "revoked" {
				query = `UPDATE credentials SET revoked=1 WHERE token_sha256=?`
			}
			if _, err = f.store.DB().Exec(query, hash); err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				var e *core.Error
				want := "forbidden"
				if mode == "revoked" {
					want = "unauthorized"
				}
				if !errors.As(err, &e) || e.Code != want {
					t.Errorf("want %s, got %v", want, err)
				}
			}
			_, err = f.service.Configuration(t.Context(), scope)
			check(err)
			_, err = f.service.Preview(t.Context(), scope, Approved{2, true, input.Manifest})
			check(err)
			_, err = f.service.Apply(t.Context(), scope, "admin-apply", input, preview.SHA256)
			check(err)
			_, err = f.service.Unpublish(t.Context(), scope, "admin-unpublish", 1)
			check(err)
			next := Approved{2, true, input.Manifest}
			_, err = f.service.Apply(t.Context(), scope, "new-apply", next, f.service.previewHash(scope, next))
			check(err)
			_, err = f.service.Unpublish(t.Context(), scope, "new-unpublish", 2)
			check(err)
			var enabled bool
			var revision int64
			if err = f.store.DB().QueryRow(`SELECT enabled,revision FROM project_publications WHERE project_id=?`, f.project).Scan(&enabled, &revision); err != nil || enabled || revision != 2 {
				t.Fatal("stale admin changed publication", enabled, revision, err)
			}
		})
	}
}
