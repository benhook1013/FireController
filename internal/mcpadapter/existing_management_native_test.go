package mcpadapter_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/publicsite"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExistingManagementOfficialSDKNativePublicationAndCurrentAuthority(t *testing.T) {
	f := presentationSDKFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	publicsite.New(f.store, nil).Register(mux, t.Context())
	service := httptest.NewServer(mux)
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	replace := map[string]any{"document": client.ReplacePresentationRequest{ExpectedRevision: 0, Presentation: client.PresentationConfig{Name: "Selected display"}}, "idempotency_key": "appearance-one"}
	var first client.PresentationReceipt
	decodeResult(t, call(t, session, "presentation_replace", replace), &first)
	if first.ProjectID != f.project || first.Revision != 1 {
		t.Fatal(first)
	}
	var replay client.PresentationReceipt
	decodeResult(t, call(t, session, "presentation_replace", replace), &replay)
	if replay != first {
		t.Fatal("lost-response replay changed mutation", replay)
	}
	var revision client.PresentationRevision
	decodeResult(t, call(t, session, "presentation_revision_read", map[string]any{"revision": 1}), &revision)
	if revision.State.Name != "Selected display" || revision.Revision != 1 {
		t.Fatal(revision)
	}
	input, err := client.DecodePublicationInput([]byte(`{"expected_revision":0,"enabled":true,"manifest":{"slug":"published-example","presentation":{"name":"Approved public display"},"description":"Explicitly approved public text"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var preview client.PublicationPreview
	decodeResult(t, call(t, session, "publication_preview", map[string]any{"document": input}), &preview)
	if preview.SHA256 == "" {
		t.Fatal("preview lost native guard")
	}
	apply := map[string]any{"document": input, "preview_sha256": preview.SHA256, "idempotency_key": "publish-one"}
	var published client.PublicationReceipt
	decodeResult(t, call(t, session, "publication_apply", apply), &published)
	if published.Revision != 1 {
		t.Fatal(published)
	}
	var configuration client.PublicationConfiguration
	decodeResult(t, call(t, session, "publication_configuration_read", map[string]any{}), &configuration)
	if configuration.Revision != 1 || !configuration.Enabled || configuration.Manifest == nil || configuration.Manifest.Description != input.Manifest.Description {
		t.Fatal("publication selection changed", configuration)
	}
	var disabled client.PublicationReceipt
	decodeResult(t, call(t, session, "publication_unpublish", map[string]any{"expected_revision": 1, "idempotency_key": "unpublish-one"}), &disabled)
	if disabled.Revision != 2 {
		t.Fatal(disabled)
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	diagnosticResult(t, call(t, session, "presentation_replace", replace), "forbidden", false)
	var retained client.PresentationRevision
	decodeResult(t, call(t, session, "presentation_revision_read", map[string]any{"revision": 1}), &retained)
	if retained.State.Name != "Selected display" {
		t.Fatal("downgrade changed retained content")
	}
}
