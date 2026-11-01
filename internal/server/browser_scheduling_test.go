package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestBrowserSchedulingCookieAuthorityAndExplicitAuthorizer(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	worker := f.worker(t, project, "Authorizer")
	other := f.project(t, "owner/two")
	foreignWorker := f.worker(t, other, "Foreign")
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	e, err := reviewexec.New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Register(f.handler.(*http.ServeMux))
	reader, writer, manager := "browser-scheduling-reader-token-12345", "browser-scheduling-writer-token-12345", "browser-scheduling-manager-token-12345"
	f.grant(t, project, reader, false)
	f.grant(t, project, writer, true)
	sum := sha256.Sum256([]byte(manager))
	require(t, request(f.handler, "POST", "/api/v1/projects/"+project+"/credentials", f.admin, core.NewID("key"), "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":["manage"]}`), 200)
	login := func(token, id string) *http.Cookie {
		w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", token, "", `{"session_id":"`+id+`"}`, "localhost")
		require(t, w, 200)
		return w.Result().Cookies()[0]
	}
	rid, wid, mid := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	rc, wc, mc := login(reader, rid), login(writer, wid), login(manager, mid)
	base := "/api/v1/projects/" + project + "/reviews/"
	providers := f.calls.Load()
	call := func(method, path, id string, cookie *http.Cookie, worker, key, body string) map[string]any {
		return require(t, serveBrowserAction(f, browserActionRequest(f, method, base+path, id, cookie, worker, key, body)), 200)
	}
	if got := call("GET", "scheduling", rid, rc, "", "", ""); got["revision"] != float64(0) || got["automatic_hosted"] != false {
		t.Fatal(got)
	}
	pause := `{"expected_revision":0,"paused":true,"reason":"Prepared fixes"}`
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"pulls/7/scheduling", rid, rc, "", "reader-pause", pause)), 403)
	call("POST", "pulls/7/scheduling", wid, wc, "", "writer-pause", pause)
	first := call("GET", "pulls/7/scheduling", rid, rc, "", "", "")
	if first["paused"] != true || first["reason"] != "Prepared fixes" {
		t.Fatal(first)
	}
	before := count(t, f.store, "events")
	call("POST", "pulls/7/scheduling", wid, wc, "", "writer-pause", pause)
	if count(t, f.store, "events") != before {
		t.Fatal("exact replay duplicated event")
	}
	enable := `{"expected_revision":0,"automatic_hosted":true}`
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"scheduling", wid, wc, worker, "writer-enable", enable)), 403)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"scheduling", mid, mc, "", "missing-worker", enable)), 404)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"scheduling", mid, mc, foreignWorker, "foreign-worker", enable)), 404)
	bad := browserActionRequest(f, "POST", base+"scheduling", mid, mc, worker, "bad-origin", enable)
	bad.Header.Set("Origin", "https://other.invalid")
	require(t, serveBrowserAction(f, bad), 403)
	call("POST", "scheduling", mid, mc, worker, "manager-enable", enable)
	settings := call("GET", "scheduling", rid, rc, "", "", "")
	if settings["automatic_hosted"] != true || settings["authorizing_worker_id"] != worker {
		t.Fatal(settings)
	}
	var principal, actor, authorizer string
	if err := f.store.DB().QueryRow(`SELECT principal_id FROM credentials WHERE token_sha256=?`, hex.EncodeToString(sum[:])).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT actor_id,worker_id FROM review_scheduling WHERE project_id=?`, project).Scan(&actor, &authorizer); err != nil || actor != principal || authorizer != worker {
		t.Fatal("human principal was replaced", err)
	}
	call("POST", "pulls/7/scheduling", wid, wc, "", "resume", `{"expected_revision":1,"paused":false}`)
	if got := call("GET", "pulls/7/scheduling", rid, rc, "", "", ""); got["paused"] != false || got["reason"] != "" {
		t.Fatal(got)
	}
	if f.calls.Load() != providers || count(t, f.store, "review_operations") != 0 {
		t.Fatal("scheduling contacted provider or submitted review")
	}
}
