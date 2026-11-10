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

func publicationScope(t *testing.T, f *publicationFixture, token string, administrative bool) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", f.base()+"/publication", nil).WithContext(t.Context())
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", f.worker)
	var scope core.Scope
	var err error
	if administrative {
		scope, err = f.store.AdminScope(r)
	} else {
		scope, err = f.store.Scope(r, "read")
	}
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func requirePublicationError(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Errorf("want %s, got %v", code, err)
	}
}

func TestPublicationRequiresManagementAtServiceBoundary(t *testing.T) {
	for _, permission := range []string{"read", "write"} {
		t.Run(permission, func(t *testing.T) {
			f := newPublicationFixture(t)
			if permission == "read" {
				if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE project_id=?`, f.project); err != nil {
					t.Fatal(err)
				}
			}
			scope := publicationScope(t, f, f.reader, false)
			input := Approved{0, true, testPublicManifest()}
			preview := f.service.previewHash(scope, input)
			before := publicationCounts(t, f.store)
			base := f.base() + "/publication"
			for _, call := range []struct {
				method, path string
				body         any
			}{
				{"GET", base, nil},
				{"POST", base + "/preview", input},
				{"PUT", base, map[string]any{"expected_revision": 0, "enabled": true, "manifest": input.Manifest, "preview_sha256": preview}},
				{"POST", base + "/unpublish", map[string]any{"expected_revision": 0}},
			} {
				t.Run(call.method+call.path, func(t *testing.T) {
					w := f.call(t, call.method, call.path, f.reader, "worker-publication", call.body)
					if w.Code != 403 {
						t.Fatalf("worker publication returned %d: %s", w.Code, w.Body.String())
					}
				})
			}
			// Direct service entry points must not rely on an HTTP handler to
			// strengthen an otherwise authenticated ordinary project scope.
			_, err := f.service.Configuration(t.Context(), scope)
			requirePublicationError(t, err, "forbidden")
			_, err = f.service.Preview(t.Context(), scope, input)
			requirePublicationError(t, err, "forbidden")
			_, err = f.service.Unpublish(t.Context(), scope, "worker-unpublish", 0)
			requirePublicationError(t, err, "forbidden")
			_, err = f.service.Apply(t.Context(), scope, "worker-apply", input, preview)
			requirePublicationError(t, err, "forbidden")
			if !reflectCounts(before, publicationCounts(t, f.store)) {
				t.Fatal("unauthorized publication changed storage")
			}
		})
	}
}

func TestPublicationRechecksCurrentManagementBeforeReceiptReplay(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
		administrator     bool
	}{
		{"manager-demoted", `UPDATE grants SET can_manage=0 WHERE credential_id=(SELECT id FROM credentials WHERE token_sha256=?)`, "forbidden", false},
		{"manager-grant-removed", `DELETE FROM grants WHERE credential_id=(SELECT id FROM credentials WHERE token_sha256=?)`, "forbidden", false},
		{"manager-revoked", `UPDATE credentials SET revoked=1 WHERE token_sha256=?`, "unauthorized", false},
		{"administrator-demoted", `UPDATE credentials SET is_admin=0 WHERE token_sha256=?`, "forbidden", true},
		{"administrator-revoked", `UPDATE credentials SET revoked=1 WHERE token_sha256=?`, "unauthorized", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			token := f.reader
			if tc.administrator {
				token = f.admin
			} else if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE project_id=?`, f.project); err != nil {
				t.Fatal(err)
			}
			ordinary := publicationScope(t, f, token, false)
			administrative := publicationScope(t, f, token, true)
			input := Approved{0, true, testPublicManifest()}
			// Both current project managers and instance administrators can use
			// the service, even when authenticated through an ordinary scope.
			configuration, err := f.service.Configuration(t.Context(), ordinary)
			if err != nil || configuration.Revision != 0 || configuration.Enabled {
				t.Fatal("initial authorized configuration", configuration, err)
			}
			preview, err := f.service.Preview(t.Context(), ordinary, input)
			if err != nil {
				t.Fatal(err)
			}
			applied, err := f.service.Apply(t.Context(), ordinary, "authorized-apply", input, preview.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := f.service.Apply(t.Context(), ordinary, "authorized-apply", input, preview.SHA256)
			if err != nil || !bytes.Equal(applied, replay) {
				t.Fatal("authorized apply replay", err)
			}
			unpublished, err := f.service.Unpublish(t.Context(), ordinary, "authorized-unpublish", 1)
			if err != nil {
				t.Fatal(err)
			}
			replay, err = f.service.Unpublish(t.Context(), ordinary, "authorized-unpublish", 1)
			if err != nil || !bytes.Equal(unpublished, replay) {
				t.Fatal("authorized unpublish replay", err)
			}
			// Recovery of an old apply does not republish withdrawn content.
			replay, err = f.service.Apply(t.Context(), ordinary, "authorized-apply", input, preview.SHA256)
			if err != nil || !bytes.Equal(applied, replay) {
				t.Fatal("withdrawn apply replay", err)
			}
			digest := sha256.Sum256([]byte(token))
			hash := hex.EncodeToString(digest[:])
			if tc.administrator {
				// Demotion leaves ordinary read/write available but no management.
				if _, err = f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) SELECT id,?,1,1,0 FROM credentials WHERE token_sha256=?`, f.project, hash); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.store.DB().Exec(tc.query, hash); err != nil {
				t.Fatal(err)
			}
			before := publicationCounts(t, f.store)
			for _, selection := range []struct {
				name  string
				scope core.Scope
			}{{"ordinary", ordinary}, {"administrative", administrative}} {
				t.Run(selection.name, func(t *testing.T) {
					scope := selection.scope
					_, err := f.service.Configuration(t.Context(), scope)
					requirePublicationError(t, err, tc.want)
					next := Approved{2, true, input.Manifest}
					_, err = f.service.Preview(t.Context(), scope, next)
					requirePublicationError(t, err, tc.want)
					_, err = f.service.Apply(t.Context(), scope, "authorized-apply", input, preview.SHA256)
					requirePublicationError(t, err, tc.want)
					_, err = f.service.Unpublish(t.Context(), scope, "authorized-unpublish", 1)
					requirePublicationError(t, err, tc.want)
					_, err = f.service.Unpublish(t.Context(), scope, "new-unpublish", 2)
					requirePublicationError(t, err, tc.want)
					_, err = f.service.Apply(t.Context(), scope, "new-apply", next, f.service.previewHash(scope, next))
					requirePublicationError(t, err, tc.want)
				})
			}
			if !reflectCounts(before, publicationCounts(t, f.store)) {
				t.Fatal("stale authority changed publication storage")
			}
			var enabled bool
			var revision int64
			if err = f.store.DB().QueryRow(`SELECT enabled,revision FROM project_publications WHERE project_id=?`, f.project).Scan(&enabled, &revision); err != nil || enabled || revision != 2 {
				t.Fatal("stale authority changed publication", enabled, revision, err)
			}
		})
	}
}

func TestPublicationRetainsProjectManagementAfterInstanceDemotion(t *testing.T) {
	f := newPublicationFixture(t)
	scope := publicationScope(t, f, f.admin, true)
	digest := sha256.Sum256([]byte(f.admin))
	hash := hex.EncodeToString(digest[:])
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) SELECT id,?,1,1,1 FROM credentials WHERE token_sha256=?`, f.project, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE token_sha256=?`, hash); err != nil {
		t.Fatal(err)
	}
	input := Approved{0, true, testPublicManifest()}
	preview, err := f.service.Preview(t.Context(), scope, input)
	if err != nil {
		t.Fatal("retained project management cannot preview", err)
	}
	applied, err := f.service.Apply(t.Context(), scope, "manager-apply", input, preview.SHA256)
	if err != nil {
		t.Fatal("retained project management cannot apply", err)
	}
	configuration, err := f.service.Configuration(t.Context(), scope)
	if err != nil || !configuration.Enabled || configuration.Revision != 1 {
		t.Fatal("retained project management cannot read configuration", configuration, err)
	}
	unpublished, err := f.service.Unpublish(t.Context(), scope, "manager-unpublish", 1)
	if err != nil {
		t.Fatal("retained project management cannot unpublish", err)
	}
	replay, err := f.service.Apply(t.Context(), scope, "manager-apply", input, preview.SHA256)
	if err != nil || !bytes.Equal(applied, replay) {
		t.Fatal("retained project management cannot replay apply", err)
	}
	replay, err = f.service.Unpublish(t.Context(), scope, "manager-unpublish", 1)
	if err != nil || !bytes.Equal(unpublished, replay) {
		t.Fatal("retained project management cannot replay unpublish", err)
	}
	configuration, err = f.service.Configuration(t.Context(), scope)
	if err != nil || configuration.Enabled || configuration.Revision != 2 {
		t.Fatal("retained manager recovery republished content", configuration, err)
	}
}
