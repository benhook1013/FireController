package reviewexec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoundControlsHTTPOrdinaryWriterGrantCloseReopenAndReplay(t *testing.T) {
	f := roundFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/rounds/2/hosted"
	grant := `{"expected_revision":0,"action":"grant","minimum":0,"maximum":3}`
	first := f.request("POST", path, f.writer, "writer-grant", grant)
	evidenceHTTPRequire(t, first, 200, "")
	if got := roundRead(t, f, 2, "hosted"); got.Maximum != 3 || got.Closed || got.ActorID != "exec-writer-principal" {
		t.Fatal("ordinary writer grant not attributed", got)
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.writer, "writer-close", `{"expected_revision":1,"action":"close"}`), 200, "")
	if !roundRead(t, f, 2, "hosted").Closed {
		t.Fatal("ordinary writer did not close channel")
	}
	before := roundTables(t, f)
	replay := f.request("POST", path, f.writer, "writer-grant", grant)
	if replay.Code != 200 || replay.Body.String() != first.Body.String() || roundTables(t, f) != before {
		t.Fatal("writer original-key replay changed closed control", replay.Body.String())
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.writer, "writer-reopen", `{"expected_revision":2,"action":"grant","minimum":1,"maximum":2}`), 200, "")
	if got := roundRead(t, f, 2, "hosted"); got.Closed || got.Epoch != 2 || got.Minimum != 1 {
		t.Fatal("ordinary writer did not reopen", got)
	}
	before = roundTables(t, f)
	for _, token := range []string{f.reader, f.other} {
		evidenceHTTPRequire(t, f.request("POST", path, token, "denied", grant), 403, "forbidden")
	}
	// Read-only browser sessions cannot inherit write authority from the
	// mutation endpoint.
	login := httptest.NewRequest("POST", "http://localhost/api/v1/projects/"+attemptProject+"/browser-session", nil)
	login.SetPathValue("project_id", attemptProject)
	login.Header.Set("Authorization", "Bearer "+f.reader)
	login.Header.Set("X-FC-Instance", f.store.InstanceID())
	id := strings.Repeat("b", 32)
	_, cookie, err := f.store.CreateBrowserSession(login, id)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(grant))
	request.Header.Set("X-FC-Browser-Session", id)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	request.Header.Set("Idempotency-Key", "writer-grant")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	evidenceHTTPRequire(t, response, 403, "forbidden")
	if roundTables(t, f) != before {
		t.Fatal("reader, foreign writer or read-only cookie changed controls")
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.writer, "writer-grant", grant), 403, "forbidden")
	evidenceHTTPRequire(t, f.request("POST", path, f.writer, "after-revoke", `{"expected_revision":3,"action":"close"}`), 403, "forbidden")
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request(http.MethodPost, path, f.writer, "writer-grant", grant), 401, "unauthorized")
	if roundTables(t, f) != before {
		t.Fatal("revoked writer changed controls or replayed receipt")
	}
}
