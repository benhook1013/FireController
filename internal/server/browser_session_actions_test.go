package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/presentation"
)

func browserActionsFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	if err := inbox.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := presentation.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := f.handler.(*http.ServeMux)
	inbox.Register(mux, f.store)
	presentation.Register(mux, f.store)
	f.handler = mux
	return f
}

func browserActionsLogin(t *testing.T, f *fixture, project, token, id string, admin bool) *http.Cookie {
	t.Helper()
	w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", token, "", `{"session_id":"`+id+`","allow_writes":true}`, "localhost")
	v := require(t, w, 200)
	if len(v) != 6 || v["allow_writes"] != true || v["can_edit_presentation"] != admin || v["session_id"] != id || v["project_id"] != project || v["instance_id"] != f.store.InstanceID() {
		t.Fatal("write session capability mismatch")
	}
	if strings.Contains(w.Body.String(), token) || len(w.Result().Cookies()) != 1 {
		t.Fatal("unsafe connection response")
	}
	return w.Result().Cookies()[0]
}

func browserActionRequest(f *fixture, method, path, id string, cookie *http.Cookie, worker, key, body string) *http.Request {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("X-FC-Browser-Session", id)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("Origin", "http://localhost")
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.AddCookie(cookie)
	return r
}

func serveBrowserAction(f *fixture, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestBrowserSessionActionsSendReplyHandlingAndExactRetry(t *testing.T) {
	f := browserActionsFixture(t)
	p := f.project(t, "owner/one")
	a, b, outsider := f.worker(t, p, "Sender"), f.worker(t, p, "Recipient"), f.worker(t, p, "Other")
	other := f.project(t, "owner/two")
	otherWorker := f.worker(t, other, "Other project")
	token := "synthetic-browser-actions-write-token-1234567"
	f.grant(t, p, token, true)
	id := strings.Repeat("a", 32)
	cookie := browserActionsLogin(t, f, p, token, id, false)
	base := "/api/v1/projects/" + p
	body := `{"recipient_worker_id":"` + b + `","body":"Synthetic browser message"}`
	key := "browser-message-key"
	calls := f.calls.Load()
	w := serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, a, key, body))
	sent := require(t, w, 201)
	msg, conv := sent["id"].(string), sent["conversation_id"].(string)
	if sent["outcome"] != "sent" {
		t.Fatal("send receipt mismatch")
	}
	beforeEvents, beforeReceipts := count(t, f.store, "events"), count(t, f.store, "receipts")
	retried := serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, a, key, body))
	require(t, retried, 201)
	if retried.Body.String() != w.Body.String() || count(t, f.store, "inbox_messages") != 1 || count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
		t.Fatal("exact retry duplicated message or changed receipt")
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, a, key, strings.Replace(body, "Synthetic", "Changed", 1))), 409)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, b, key, body)), 409)
	// Reads have no implicit seen/ack side effect.
	require(t, serveBrowserAction(f, browserActionRequest(f, "GET", base+"/conversations/"+conv, id, cookie, b, "", "")), 200)
	var seen, ack int
	if err := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?`, msg).Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("conversation read changed handling state", err)
	}
	for _, worker := range []string{a, outsider} {
		require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages/"+msg+"/ack", id, cookie, worker, core.NewID("key"), `{}`)), 403)
	}
	handled := require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages/"+msg+"/ack", id, cookie, b, "browser-ack", `{}`)), 200)
	if handled["outcome"] != "acknowledged" {
		t.Fatal("ack receipt mismatch")
	}
	if err := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?`, msg).Scan(&seen, &ack); err != nil || seen != 1 || ack != 1 {
		t.Fatal("ack did not establish seen and acknowledged", err)
	}
	reply := `{"recipient_worker_id":"` + a + `","body":"Synthetic reply","reply_to_message_id":"` + msg + `"}`
	response := require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, b, "browser-reply", reply)), 201)
	if response["conversation_id"] != conv {
		t.Fatal("reply detached from conversation")
	}
	response = require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages/"+response["id"].(string)+"/seen", id, cookie, a, "browser-seen", `{}`)), 200)
	if response["outcome"] != "seen" {
		t.Fatal("seen receipt mismatch")
	}
	for _, test := range []struct{ worker, key, body string }{
		{"", "missing-worker", body}, {a, "", body},
	} {
		require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, test.worker, test.key, test.body)), 400)
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, otherWorker, "wrong-worker", body)), 404)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, a, "wrong-recipient", `{"recipient_worker_id":"`+otherWorker+`","body":"Synthetic"}`)), 404)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", "/api/v1/projects/"+other+"/messages", id, cookie, otherWorker, "wrong-project", body)), 403)
	if f.calls.Load() != calls {
		t.Fatal("browser actions invoked provider")
	}
}

func TestBrowserSessionActionsCSRFRouteAllowlistAndReadPreservation(t *testing.T) {
	f := browserActionsFixture(t)
	p := f.project(t, "owner/one")
	a, b := f.worker(t, p, "Sender"), f.worker(t, p, "Recipient")
	id := strings.Repeat("b", 32)
	cookie := browserActionsLogin(t, f, p, f.admin, id, true)
	base := "/api/v1/projects/" + p
	body := `{"recipient_worker_id":"` + b + `","body":"Synthetic"}`
	before := count(t, f.store, "receipts")
	for _, change := range []string{"missing_origin", "null_origin", "other_origin", "wrong_port", "duplicate_origin", "cross_site", "duplicate_site", "forwarded", "missing_instance", "wrong_instance", "duplicate_instance", "duplicate_worker"} {
		t.Run(change, func(t *testing.T) {
			r := browserActionRequest(f, "POST", base+"/messages", id, cookie, a, core.NewID("key"), body)
			status := 403
			switch change {
			case "missing_origin":
				r.Header.Del("Origin")
			case "null_origin":
				r.Header.Set("Origin", "null")
			case "other_origin":
				r.Header.Set("Origin", "http://other.example")
			case "wrong_port":
				r.Header.Set("Origin", "http://localhost:41574")
			case "duplicate_origin":
				r.Header.Add("Origin", "http://localhost")
			case "cross_site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "duplicate_site":
				r.Header.Add("Sec-Fetch-Site", "same-origin")
				r.Header.Add("Sec-Fetch-Site", "same-origin")
			case "forwarded":
				r.Host = "controller.example"
				r.Header.Set("X-Forwarded-Host", "localhost")
				r.Header.Set("X-Forwarded-Proto", "http")
				// Secure cookie name follows actual host; copy the same signed
				// claim using a public cookie is covered by core origin tests.
				status = 401
			case "missing_instance":
				r.Header.Del("X-FC-Instance")
				status = 400
			case "wrong_instance":
				r.Header.Set("X-FC-Instance", "instance_other")
				status = 409
			case "duplicate_instance":
				r.Header.Add("X-FC-Instance", f.store.InstanceID())
				status = 400
			case "duplicate_worker":
				r.Header.Add("X-FC-Worker", b)
				status = 400
			}
			require(t, serveBrowserAction(f, r), status)
		})
	}
	for _, route := range []struct{ method, path, body string }{
		{"POST", base + "/workers", `{"name":"blocked"}`},
		{"POST", base + "/credentials", `{}`},
		{"DELETE", base + "/credentials/synthetic", `{}`},
		{"POST", "/api/v1/projects", `{}`},
	} {
		require(t, serveBrowserAction(f, browserActionRequest(f, route.method, route.path, id, cookie, a, core.NewID("key"), route.body)), 403)
	}
	require(t, browserRequest(f, "GET", "/api/v1/projects", "", id, "", "localhost", cookie), 403)
	require(t, browserRequest(f, "GET", base, "", id, "", "localhost", cookie), 200)
	if count(t, f.store, "receipts") != before || count(t, f.store, "inbox_messages") != 0 {
		t.Fatal("forbidden action committed state")
	}
	readonly := browserLogin(t, f, p, f.admin, strings.Repeat("c", 32), "localhost")
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", strings.Repeat("c", 32), readonly, a, "readonly-action", body)), 403)
}

func TestBrowserSessionActionsAdminPresentationAndCurrentCapabilities(t *testing.T) {
	f := browserActionsFixture(t)
	p := f.project(t, "owner/one")
	worker := f.worker(t, p, "Editor")
	id := strings.Repeat("d", 32)
	cookie := browserActionsLogin(t, f, p, f.admin, id, true)
	base := "/api/v1/projects/" + p
	current := require(t, browserRequest(f, "GET", base+"/browser-session", "", id, "", "localhost", cookie), 200)
	if len(current) != 6 || current["allow_writes"] != true || current["can_edit_presentation"] != true {
		t.Fatal("current admin capability mismatch")
	}
	require(t, browserRequest(f, "GET", base+"/browser-session", f.admin, "", "", "localhost"), 401)
	require(t, browserRequest(f, "GET", base+"/browser-session?extra=1", "", id, "", "localhost", cookie), 400)
	body := `{"expected_revision":0,"presentation":{"name":"New title","brand_name":"FireController","tagline":"Project workspace","headings":{},"theme":{}}}`
	w := serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, worker, "appearance-key", body))
	value := require(t, w, 200)
	if value["outcome"] != "updated" || value["revision"] != float64(1) {
		t.Fatal("presentation edit not persisted")
	}
	retry := serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, worker, "appearance-key", body))
	require(t, retry, 200)
	if retry.Body.String() != w.Body.String() {
		t.Fatal("presentation replay changed")
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, worker, "appearance-stale", body)), 409)
	require(t, serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, "", "appearance-no-worker", body)), 400)
	token := "synthetic-browser-presentation-write-token-1234567"
	credential := f.grant(t, p, token, true)
	id = strings.Repeat("e", 32)
	cookie = browserActionsLogin(t, f, p, token, id, false)
	require(t, serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, worker, "appearance-no-admin", body)), 403)
	require(t, browserRequest(f, "GET", base+"/presentation", "", id, "", "localhost", cookie), 200)
	// A later credential promotion cannot broaden an already connected tab.
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id=?`, credential); err != nil {
		t.Fatal(err)
	}
	current = require(t, browserRequest(f, "GET", base+"/browser-session", "", id, "", "localhost", cookie), 200)
	if current["allow_writes"] != true || current["can_edit_presentation"] != false {
		t.Fatal("credential promotion silently widened old cookie")
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", id, cookie, worker, "appearance-promoted-old-cookie", body)), 403)
	newID := strings.Repeat("a", 32)
	adminCookie := browserActionsLogin(t, f, p, token, newID, true)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE id=?`, credential); err != nil {
		t.Fatal(err)
	}
	current = require(t, browserRequest(f, "GET", base+"/browser-session", "", newID, "", "localhost", adminCookie), 200)
	if current["allow_writes"] != true || current["can_edit_presentation"] != false {
		t.Fatal("current capabilities retained removed administrator authority")
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, "PUT", base+"/presentation", newID, adminCookie, worker, "appearance-demoted-cookie", body)), 403)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id=?`, credential); err != nil {
		t.Fatal(err)
	}
	current = require(t, browserRequest(f, "GET", base+"/browser-session", "", id, "", "localhost", cookie), 200)
	if current["allow_writes"] != false || current["can_edit_presentation"] != false {
		t.Fatal("effective current capabilities ignored write-grant loss")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id=?`, credential); err != nil {
		t.Fatal(err)
	}
	require(t, browserRequest(f, "GET", base+"/browser-session", "", id, "", "localhost", cookie), 401)
	require(t, serveBrowserAction(f, browserActionRequest(f, "POST", base+"/messages", id, cookie, worker, "after-revoke", `{}`)), 401)
}

func TestBrowserSessionActionsStrictCreationAndReadOnlyCredential(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	id := strings.Repeat("f", 32)
	token := "synthetic-browser-readonly-action-token-1234567"
	f.grant(t, p, token, false)
	for _, value := range []string{`null`, `"true"`, `1`, `{}`, `[]`} {
		require(t, browserRequest(f, "POST", "/api/v1/projects/"+p+"/browser-session", f.admin, "", `{"session_id":"`+id+`","allow_writes":`+value+`}`, "localhost"), 400)
	}
	for _, extra := range []string{`,"allow_writes":true`, `,"Allow_Writes":true`, `,"unexpected":true`} {
		require(t, browserRequest(f, "POST", "/api/v1/projects/"+p+"/browser-session", f.admin, "", `{"session_id":"`+id+`","allow_writes":false`+extra+`}`, "localhost"), 400)
	}
	w := browserRequest(f, "POST", "/api/v1/projects/"+p+"/browser-session", token, "", `{"session_id":"`+id+`","allow_writes":true}`, "localhost")
	require(t, w, 403)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("denied write connection installed cookie")
	}
	cookie := browserLogin(t, f, p, token, id, "localhost")
	current := require(t, browserRequest(f, "GET", "/api/v1/projects/"+p+"/browser-session", "", id, "", "localhost", cookie), 200)
	if raw, err := json.Marshal(current); err != nil || current["allow_writes"] != false || current["can_edit_presentation"] != false || strings.Contains(string(raw), token) || strings.Contains(string(raw), cookie.Value) {
		t.Fatal("unsafe readonly capabilities")
	}
}
