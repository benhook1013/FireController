package opvars_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/opvars"
	"github.com/benhook1013/FireController/internal/server"
)

type fixture struct {
	t                                *testing.T
	store                            *core.Store
	mux                              *http.ServeMux
	admin, project, other            string
	reader, writer, manager, outside string
	keys                             int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err = opvars.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, admin: strings.TrimSpace(string(token))}
	mux := http.NewServeMux()
	mux.Handle("/", server.New(store, server.WithRepositoryResolver(func(_ context.Context, name string) (server.RepositoryIdentity, error) {
		id := int64(1)
		if strings.HasSuffix(name, "two") {
			id = 2
		}
		return server.RepositoryIdentity{ID: id, FullName: name}, nil
	})))
	opvars.Register(mux, store)
	f.mux = mux
	f.project = f.newProject("one")
	f.other = f.newProject("two")
	f.reader = f.grant(f.project, "reader", "read")
	f.writer = f.grant(f.project, "writer", "read", "write")
	f.manager = f.grant(f.project, "manager", "manage")
	f.outside = f.grant(f.other, "outside", "read")
	return f
}

func (f *fixture) call(method, path, token string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if method != http.MethodGet {
		f.keys++
		r.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", f.keys))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *fixture) newProject(name string) string {
	w := f.call("POST", "/api/v1/projects", f.admin, map[string]string{"name": name, "repository": "owner/" + name})
	expect(f.t, w, 200)
	var v struct {
		ID string `json:"id"`
	}
	decode(f.t, w, &v)
	return v.ID
}

func (f *fixture) grant(project, token string, permissions ...string) string {
	sum := sha256.Sum256([]byte(token))
	w := f.call("POST", "/api/v1/projects/"+project+"/credentials", f.admin, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions})
	expect(f.t, w, 200)
	return token
}

func (f *fixture) path(project, key string) string {
	p := "/api/v1/projects/" + project + "/vars"
	if key != "" {
		p += "/" + key
	}
	return p
}

func (f *fixture) set(token, project, key, scope string, value any, extra map[string]any) *httptest.ResponseRecorder {
	body := map[string]any{"scope": scope, "value": value}
	for k, v := range extra {
		body[k] = v
	}
	return f.call("PUT", f.path(project, key), token, body)
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
	}
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatal(err)
	}
}

func receipt(t *testing.T, w *httptest.ResponseRecorder) opvars.Receipt {
	t.Helper()
	expect(t, w, 200)
	var r opvars.Receipt
	decode(t, w, &r)
	return r
}

func effective(t *testing.T, f *fixture, token, project string) map[string]opvars.Variable {
	t.Helper()
	w := f.call("GET", f.path(project, ""), token, nil)
	expect(t, w, 200)
	var list opvars.List
	decode(t, w, &list)
	out := map[string]opvars.Variable{}
	for _, v := range list.Variables {
		out[v.Key] = v
	}
	if list.Variables == nil {
		t.Fatal("variables must be an explicit list")
	}
	return out
}

func TestInstanceValueOverrideAndUnset(t *testing.T) {
	f := setup(t)
	if got := effective(t, f, f.reader, f.project); len(got) != 0 {
		t.Fatal("expected empty", got)
	}
	if r := receipt(t, f.set(f.admin, f.project, "provider_mode", "instance", "both", nil)); r.Revision != 1 || r.Outcome != "updated" || r.Scope != "instance" {
		t.Fatal(r)
	}
	if r := receipt(t, f.set(f.admin, f.project, "provider_mode", "instance", "both", nil)); r.Revision != 1 || r.Outcome != "unchanged" {
		t.Fatal("repeat must be unchanged", r)
	}
	receipt(t, f.set(f.admin, f.project, "provider_mode", "instance", "codex-low", nil))
	// Both projects see the instance value through any read credential.
	for _, c := range []struct{ token, project string }{{f.reader, f.project}, {f.outside, f.other}} {
		got := effective(t, f, c.token, c.project)["provider_mode"]
		if got.Value != "codex-low" || got.Scope != "instance" || got.Revision != 2 {
			t.Fatal("instance value", got)
		}
	}
	// A project override applies only to that project.
	receipt(t, f.set(f.admin, f.project, "provider_mode", "project", "claude-low", nil))
	if got := effective(t, f, f.reader, f.project)["provider_mode"]; got.Value != "claude-low" || got.Scope != "project" || got.Revision != 1 {
		t.Fatal("override", got)
	}
	if got := effective(t, f, f.outside, f.other)["provider_mode"]; got.Value != "codex-low" {
		t.Fatal("override leaked", got)
	}
	w := f.call("GET", f.path(f.project, "provider_mode"), f.reader, nil)
	expect(t, w, 200)
	var detail opvars.Detail
	decode(t, w, &detail)
	if !detail.Set || detail.Value != "claude-low" || detail.Scope != "project" || detail.Instance == nil || detail.Instance.Value != "codex-low" || detail.Instance.Revision != 2 || detail.Project == nil || detail.Project.Revision != 1 {
		t.Fatal("detail", w.Body.String())
	}
	// Unsetting the override falls back to the instance value.
	if r := receipt(t, f.set(f.admin, f.project, "provider_mode", "project", nil, nil)); r.Revision != 2 || r.Outcome != "updated" {
		t.Fatal(r)
	}
	if r := receipt(t, f.set(f.admin, f.project, "provider_mode", "project", nil, nil)); r.Outcome != "unchanged" || r.Revision != 2 {
		t.Fatal(r)
	}
	if got := effective(t, f, f.reader, f.project)["provider_mode"]; got.Value != "codex-low" || got.Scope != "instance" {
		t.Fatal("fallback", got)
	}
	// Unknown keys read as unset.
	w = f.call("GET", f.path(f.project, "never_set"), f.reader, nil)
	expect(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"key":"never_set","set":false}` {
		t.Fatal(w.Body.String())
	}
	// History is per scope, newest first, with paging.
	w = f.call("GET", f.path(f.project, "provider_mode")+"/history?scope=instance&limit=1", f.reader, nil)
	expect(t, w, 200)
	var page opvars.Page
	decode(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore || page.Cursor == "" {
		t.Fatal(w.Body.String())
	}
	w = f.call("GET", f.path(f.project, "provider_mode")+"/history?scope=instance&limit=1&cursor="+page.Cursor, f.reader, nil)
	expect(t, w, 200)
	page = opvars.Page{}
	decode(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].Revision != 1 || page.Items[0].Value != "both" || page.HasMore {
		t.Fatal(w.Body.String())
	}
	w = f.call("GET", f.path(f.project, "provider_mode")+"/history?scope=project", f.reader, nil)
	expect(t, w, 200)
	page = opvars.Page{}
	decode(t, w, &page)
	if len(page.Items) != 2 || page.Items[0].Set || page.Items[0].Value != "" || page.Items[1].Value != "claude-low" {
		t.Fatal(w.Body.String())
	}
}

func TestWriteAuthorization(t *testing.T) {
	f := setup(t)
	for _, token := range []string{f.reader, f.writer, f.outside} {
		w := f.set(token, f.project, "provider_mode", "project", "x", nil)
		if w.Code != 403 && w.Code != 404 {
			t.Fatalf("non-admin write: %d %s", w.Code, w.Body.String())
		}
	}
	// A project manager may set its own override but not the instance value.
	expect(t, f.set(f.manager, f.project, "provider_mode", "instance", "x", nil), 403)
	receipt(t, f.set(f.manager, f.project, "provider_mode", "project", "x", nil))
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM operator_variable_history WHERE project_id=''`).Scan(&n); err != nil || n != 0 {
		t.Fatal("instance row written by manager", n, err)
	}
	// Reads need a credential for that project.
	expect(t, f.call("GET", f.path(f.project, ""), f.outside, nil), 403)
}

func TestValidationGuardAndBound(t *testing.T) {
	f := setup(t)
	for _, c := range []struct {
		key   string
		body  map[string]any
		token string
	}{
		{"Upper", map[string]any{"scope": "instance", "value": "x"}, f.admin},
		{"_lead", map[string]any{"scope": "instance", "value": "x"}, f.admin},
		{strings.Repeat("k", 65), map[string]any{"scope": "instance", "value": "x"}, f.admin},
		{"ok", map[string]any{"scope": "global", "value": "x"}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": ""}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": " padded"}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": "a\nb"}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": strings.Repeat("v", 241)}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": "x", "extra": true}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": "x", "expected_revision": -1}, f.admin},
		{"ok", map[string]any{"scope": "instance"}, f.admin},
		{"ok", map[string]any{"scope": "instance", "value": 5}, f.admin},
	} {
		w := f.call("PUT", f.path(f.project, c.key), c.token, c.body)
		if w.Code != 400 {
			t.Fatalf("%s %v accepted: %d %s", c.key, c.body, w.Code, w.Body.String())
		}
	}
	receipt(t, f.set(f.admin, f.project, "ok", "instance", strings.Repeat("v", 240), nil))
	w := f.set(f.admin, f.project, "ok", "instance", "y", map[string]any{"expected_revision": 0})
	expect(t, w, 409)
	if !strings.Contains(w.Body.String(), "operator_variable_revision_conflict") || !strings.Contains(w.Body.String(), `"current_revision":1`) {
		t.Fatal(w.Body.String())
	}
	if r := receipt(t, f.set(f.admin, f.project, "ok", "instance", "y", map[string]any{"expected_revision": 1})); r.Revision != 2 {
		t.Fatal(r)
	}
	for i := 1; i < 32; i++ {
		receipt(t, f.set(f.admin, f.project, fmt.Sprintf("k%d", i), "instance", "v", nil))
	}
	w = f.set(f.admin, f.project, "k32", "instance", "v", nil)
	expect(t, w, 409)
	if !strings.Contains(w.Body.String(), "operator_variable_limit") {
		t.Fatal(w.Body.String())
	}
	// Changing an existing key and project overrides are still allowed at the bound.
	receipt(t, f.set(f.admin, f.project, "k1", "instance", "changed", nil))
	receipt(t, f.set(f.admin, f.project, "k32", "project", "v", nil))
	receipt(t, f.set(f.admin, f.project, "k1", "instance", nil, nil))
	receipt(t, f.set(f.admin, f.project, "k32", "instance", "v", nil))
	if got := effective(t, f, f.reader, f.project); len(got) != 32 || got["k32"].Scope != "project" {
		t.Fatal("effective set", len(got))
	}
}
