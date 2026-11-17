package server_test

import (
	"strings"
	"testing"
)

func TestProjectHandlesCreationCollisionAndAuthenticatedResolution(t *testing.T) {
	f := setup(t)
	first := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "first-handle", "", `{"name":"Shared Project","repository":"owner/one","handle":"chosen"}`), 200)
	firstID := first["id"].(string)
	if first["handle"] != "chosen" {
		t.Fatalf("explicit handle changed: %#v", first)
	}
	calls := f.calls.Load()
	failure := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "collision", "", `{"name":"Other","repository":"owner/two","handle":"chosen"}`), 409)
	if failure["error"].(map[string]any)["code"] != "project_handle_conflict" || f.calls.Load() != calls {
		t.Fatalf("explicit collision reached provider or returned wrong error: %#v", failure)
	}
	second := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "second-handle", "", `{"name":"Shared Project","repository":"owner/two"}`), 200)
	secondHandle := second["handle"].(string)
	if secondHandle == "chosen" || !strings.HasPrefix(secondHandle, "shared-project") {
		t.Fatalf("automatic handle not derived safely: %#v", second)
	}
	read := require(t, request(f.handler, "GET", "/api/v1/projects/chosen", f.admin, "", "", ""), 200)
	if read["id"] != firstID || read["handle"] != "chosen" || read["name"] != "Shared Project" {
		t.Fatalf("handle read did not return canonical project: %#v", read)
	}
	reader := "project-handle-reader-token-123456789"
	f.grant(t, firstID, reader, false)
	if got := require(t, request(f.handler, "GET", "/api/v1/projects/chosen", reader, "", "", ""), 200); got["id"] != firstID {
		t.Fatalf("granted handle resolved incorrectly: %#v", got)
	}
	worker := require(t, request(f.handler, "POST", "/api/v1/projects/chosen/workers", f.admin, "handle-worker", "", `{"name":"Handle worker"}`), 200)
	if replay := require(t, request(f.handler, "POST", "/api/v1/projects/"+firstID+"/workers", f.admin, "handle-worker", "", `{"name":"Handle worker"}`), 200); replay["id"] != worker["id"] {
		t.Fatalf("handle and opaque routes did not share the canonical receipt namespace: first=%#v replay=%#v", worker, replay)
	}
	require(t, request(f.handler, "GET", "/api/v1/projects/"+secondHandle, reader, "", "", ""), 403)
	require(t, request(f.handler, "GET", "/api/v1/projects/unknown-handle", reader, "", "", ""), 404)
	if replay := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "first-handle", "", `{"name":"Shared Project","repository":"owner/one","handle":"chosen"}`), 200); replay["id"] != firstID || f.calls.Load() != calls+1 {
		t.Fatalf("creation replay changed canonical identity or called provider: %#v", replay)
	}
}

func TestProjectHandleBrowserSessionCanonicalIdentityAndIsolation(t *testing.T) {
	f := setup(t)
	first := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "browser-one", "", `{"name":"First","repository":"owner/one","handle":"first"}`), 200)
	second := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "browser-two", "", `{"name":"Second","repository":"owner/two","handle":"second"}`), 200)
	firstID := first["id"].(string)
	reader := "browser-handle-reader-token-123456789"
	f.grant(t, firstID, reader, false)
	sessionID := strings.Repeat("a", 32)
	login := browserRequest(f, "POST", "/api/v1/projects/first/browser-session", reader, "", `{"session_id":"`+sessionID+`"}`, "localhost")
	value := require(t, login, 200)
	if value["project_id"] != firstID {
		t.Fatalf("browser cookie retained handle instead of canonical ID: %#v", value)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("browser login did not return one private cookie")
	}
	if got := require(t, browserRequest(f, "GET", "/api/v1/projects/first", "", sessionID, "", "localhost", cookies[0]), 200); got["id"] != firstID || got["handle"] != "first" {
		t.Fatalf("handle session read changed canonical identity: %#v", got)
	}
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+firstID, "", sessionID, "", "localhost", cookies[0]), 200)
	require(t, browserRequest(f, "GET", "/api/v1/projects/second", "", sessionID, "", "localhost", cookies[0]), 403)
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+second["id"].(string), "", sessionID, "", "localhost", cookies[0]), 403)
}

func TestAuthenticatedEnabledPublicationSlugResolution(t *testing.T) {
	f := setup(t)
	first := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "publication-alias-one", "", `{"name":"First","repository":"owner/one","handle":"first"}`), 200)
	second := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "publication-alias-two", "", `{"name":"Second","repository":"owner/two","handle":"second"}`), 200)
	firstID := first["id"].(string)
	secondID := second["id"].(string)
	if _, err := f.store.DB().Exec(`CREATE TABLE project_publications (
 project_id TEXT PRIMARY KEY NOT NULL,
 revision INTEGER NOT NULL,
 slug TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 manifest_json TEXT NOT NULL,
 round_salt TEXT NOT NULL,
 generation TEXT NOT NULL,
 public_revision INTEGER NOT NULL,
 public_updated_at TEXT NOT NULL,
 refresh_error TEXT NOT NULL,
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
 );
 INSERT INTO project_publications(project_id,revision,slug,enabled,manifest_json,round_salt,generation,public_revision,public_updated_at,refresh_error)
 VALUES(?,1,'first-public',1,'{}','salt','generation',1,'2026-10-20T00:00:00Z',''),
       (?,1,'second-disabled',0,'{}','salt','generation',1,'2026-10-20T00:00:00Z','')`, firstID, secondID); err != nil {
		t.Fatal(err)
	}
	firstReader := "publication-alias-first-reader-token-123456789"
	secondReader := "publication-alias-second-reader-token-123456789"
	f.grant(t, firstID, firstReader, false)
	f.grant(t, secondID, secondReader, false)
	if got := require(t, request(f.handler, "GET", "/api/v1/projects/first-public", firstReader, "", "", ""), 200); got["id"] != firstID || got["handle"] != "first" {
		t.Fatalf("enabled publication slug did not resolve to canonical project: %#v", got)
	}
	sessionID := strings.Repeat("b", 32)
	login := browserRequest(f, "POST", "/api/v1/projects/first-public/browser-session", firstReader, "", `{"session_id":"`+sessionID+`"}`, "localhost")
	if value := require(t, login, 200); value["project_id"] != firstID {
		t.Fatalf("publication alias login retained a noncanonical project: %#v", value)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("publication alias login did not return one private cookie")
	}
	if got := require(t, browserRequest(f, "GET", "/api/v1/projects/first-public", "", sessionID, "", "localhost", cookies[0]), 200); got["id"] != firstID {
		t.Fatalf("publication alias browser read escaped canonical project: %#v", got)
	}
	require(t, request(f.handler, "GET", "/api/v1/projects/first-public", secondReader, "", "", ""), 403)
	require(t, request(f.handler, "GET", "/api/v1/projects/second-disabled", secondReader, "", "", ""), 404)
	require(t, browserRequest(f, "POST", "/api/v1/projects/second-disabled/browser-session", secondReader, "", `{"session_id":"`+strings.Repeat("c", 32)+`"}`, "localhost"), 404)
}
