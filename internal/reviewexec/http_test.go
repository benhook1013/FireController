package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type httpFixture struct {
	store                        *core.Store
	handler                      http.Handler
	admin, writer, reader, other string
}

func newHTTPFixture(t *testing.T) *httpFixture { return newHTTPFixtureWithPolicy(t, true) }

func newHTTPFixtureWithPolicy(t *testing.T, configured bool) *httpFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	phaseStarted := time.Now()
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("fixture store open after %s: %v", time.Since(phaseStarted), err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Logf("fixture store open completed in %s", time.Since(phaseStarted))
	phaseStarted = time.Now()
	if err := EnsureSchema(context.Background(), store); err != nil {
		t.Fatalf("fixture execution schema after %s: %v", time.Since(phaseStarted), err)
	}
	t.Logf("fixture execution schema completed in %s", time.Since(phaseStarted))
	phaseStarted = time.Now()
	adminBytes, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatalf("fixture seed rows after %s: %v", time.Since(phaseStarted), err)
	}
	f := &httpFixture{store: store, admin: strings.TrimSpace(string(adminBytes)), writer: "exec-writer-test-token-000000000000", reader: "exec-reader-test-token-000000000000", other: "exec-other-test-token-000000000000"}
	for i, project := range []string{"exec-project-one", "exec-project-two"} {
		if _, err := store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, project, project, 801+i, "owner/repo"); err != nil {
			t.Fatalf("fixture seed rows after %s: %v", time.Since(phaseStarted), err)
		}
	}
	if configured {
		for _, project := range []string{attemptProject, otherAttemptProject} {
			if _, err := store.DB().Exec(`INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) VALUES(?,1,?,'synthetic-admin',?)`, project, encoded(Policy{RequiredChannels: []string{"cli", "hosted"}, PausedChannels: []string{}}), stamp()); err != nil {
				t.Fatalf("fixture seed rows after %s: %v", time.Since(phaseStarted), err)
			}
		}
	}
	insertCredential := func(id, principal, token string, project string, write bool) {
		t.Helper()
		sum := sha256.Sum256([]byte(token))
		if _, err := store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,0,0)`, id, principal, hex.EncodeToString(sum[:])); err != nil {
			t.Fatalf("fixture seed rows after %s: %v", time.Since(phaseStarted), err)
		}
		canWrite := 0
		if write {
			canWrite = 1
		}
		if _, err := store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,?)`, id, project, canWrite); err != nil {
			t.Fatalf("fixture seed rows after %s: %v", time.Since(phaseStarted), err)
		}
	}
	insertCredential("exec-writer-cred", "exec-writer-principal", f.writer, "exec-project-one", true)
	insertCredential("exec-reader-cred", "exec-reader-principal", f.reader, "exec-project-one", false)
	insertCredential("exec-other-cred", "exec-other-principal", f.other, "exec-project-two", true)
	t.Logf("fixture seed rows completed in %s", time.Since(phaseStarted))
	phaseStarted = time.Now()
	e, err := New(store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatalf("fixture HTTP engine construction after %s: %v", time.Since(phaseStarted), err)
	}
	t.Logf("fixture HTTP engine construction completed in %s", time.Since(phaseStarted))
	mux := http.NewServeMux()
	e.Register(mux)
	f.handler = mux
	return f
}

func (f *httpFixture) request(method, path, token, key, body string) *httptest.ResponseRecorder {
	keys := []string(nil)
	if key != "" {
		keys = []string{key}
	}
	return f.requestWithKeys(method, path, token, keys, body)
}

func (f *httpFixture) requestWithKeys(method, path, token string, keys []string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	// Existing request/journal tests deliberately exercise immediate receipts;
	// default and explicit result waiting have their own HTTP contract proof.
	if method == http.MethodPost && strings.Contains(path, "/reviews/requests") {
		r.Header.Set("Prefer", "wait=0")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, key := range keys {
		r.Header.Add("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func assertNoExecutionMutation(t *testing.T, store *core.Store) {
	t.Helper()
	for _, table := range []string{"review_execution_config", "review_operations", "events", "receipts"} {
		var count int
		if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rejected request changed %s: %d rows", table, count)
		}
	}
}

func (f *httpFixture) configure(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":[2,4],"channels":["cli","hosted"],"paused":false}`
	return f.request(http.MethodPost, "/api/v1/projects/exec-project-one/reviews/execution/configuration", f.admin, key, body)
}

func TestExecutionHTTPRoutesAuthorizeBeforeStrictAdmission(t *testing.T) {
	f := newHTTPFixture(t)
	configPath := "/api/v1/projects/exec-project-one/reviews/execution/configuration"
	requestPath := "/api/v1/projects/exec-project-one/reviews/requests"
	operationPath := "/api/v1/projects/exec-project-one/reviews/operations/op-1"
	validConfig := `{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":[2,4],"channels":["cli","hosted"],"paused":false}`
	validRequest := `{"expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"}`

	// Authorization is evaluated before malformed request bodies or selectors.
	if got := f.request(http.MethodPost, configPath, f.reader, "unauthorized-config", `{`); got.Code != http.StatusForbidden {
		t.Fatalf("reader config: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, requestPath, f.reader, "unauthorized-request", `{`); got.Code != http.StatusForbidden {
		t.Fatalf("reader request: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, configPath, "", "", `{`); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated config: %d %s", got.Code, got.Body.String())
	}

	// Unknown fields cannot smuggle executable paths or other capabilities.
	for _, body := range []string{
		`{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":[2],"channels":["cli"],"paused":false,"executable":"/tmp/evil"}`,
		`{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":[2],"channels":["cli"],"paused":false,"credential":"secret"}`,
	} {
		if got := f.request(http.MethodPost, configPath, f.admin, "strict-config", body); got.Code != http.StatusBadRequest {
			t.Fatalf("accepted capability field: %d %s", got.Code, got.Body.String())
		}
	}
	if got := f.request(http.MethodPost, configPath+"?force=true", f.admin, "query-config", validConfig); got.Code != http.StatusBadRequest {
		t.Fatalf("config query: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, configPath+"?force=true&force=false", f.admin, "query-repeat-config", validConfig); got.Code != http.StatusBadRequest {
		t.Fatalf("repeated config query: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, configPath, f.admin, " padded-config ", validConfig); got.Code != http.StatusBadRequest {
		t.Fatalf("malformed config key: %d %s", got.Code, got.Body.String())
	}
	if got := f.requestWithKeys(http.MethodPost, configPath, f.admin, []string{"config-key-a", "config-key-b"}, validConfig); got.Code != http.StatusBadRequest {
		t.Fatalf("repeated config key: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, requestPath, f.writer, "", validRequest); got.Code != http.StatusBadRequest {
		t.Fatalf("missing request key: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, requestPath+"?unused=1", f.writer, "query-request", validRequest); got.Code != http.StatusBadRequest {
		t.Fatalf("request query: %d %s", got.Code, got.Body.String())
	}
	if got := f.requestWithKeys(http.MethodPost, requestPath, f.writer, []string{"request-key-a", "request-key-b"}, validRequest); got.Code != http.StatusBadRequest {
		t.Fatalf("repeated request key: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, requestPath, f.writer, "bad-request", strings.TrimSuffix(validRequest, "}")+`,"executable":"/tmp/evil"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("accepted request capability: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodGet, operationPath+"?verbose=true", f.reader, "", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("operation query: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodGet, "/api/v1/projects/exec-project-one/reviews/operations/bad%2Fid", f.reader, "", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid operation id: %d %s", got.Code, got.Body.String())
	}
	assertNoExecutionMutation(t, f.store)
}

func TestExecutionHTTPReceiptsReplayAndProjectScopedReads(t *testing.T) {
	f := newHTTPFixture(t)
	base := "/api/v1/projects/exec-project-one/reviews"
	firstConfig := f.configure(t, "config-key-1")
	if firstConfig.Code != http.StatusCreated {
		t.Fatalf("configure: %d %s", firstConfig.Code, firstConfig.Body.String())
	}
	var configReceipt Receipt
	if err := json.Unmarshal(firstConfig.Body.Bytes(), &configReceipt); err != nil || configReceipt.ID != "exec-project-one" || configReceipt.State != "configured" || configReceipt.Revision != 1 {
		t.Fatalf("config receipt: %v %+v", err, configReceipt)
	}
	configReplay := f.configure(t, "config-key-1")
	if configReplay.Code != http.StatusCreated || configReplay.Body.String() != firstConfig.Body.String() {
		t.Fatalf("config replay: %d %s", configReplay.Code, configReplay.Body.String())
	}
	staleConfig := `{"expected_revision":0,"capability_id":"test-capability","mode":"independent","queue":[2,4],"channels":["cli","hosted"],"paused":false}`
	if got := f.request(http.MethodPost, base+"/execution/configuration", f.admin, "config-key-2", staleConfig); got.Code != http.StatusConflict {
		t.Fatalf("stale config: %d %s", got.Code, got.Body.String())
	}

	body := `{"expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"}`
	created := f.request(http.MethodPost, base+"/requests", f.writer, "request-key-1", body)
	if created.Code != http.StatusAccepted {
		t.Fatalf("enqueue: %d %s", created.Code, created.Body.String())
	}
	var receipt Receipt
	if err := json.Unmarshal(created.Body.Bytes(), &receipt); err != nil || !identifier.MatchString(receipt.ID) || receipt.State != "queued" || receipt.Revision != 1 {
		t.Fatalf("request receipt: %v %+v", err, receipt)
	}
	if strings.Contains(created.Body.String(), "head_sha") || strings.Contains(created.Body.String(), "attempt") {
		t.Fatalf("receipt exposed operation detail: %s", created.Body.String())
	}
	replay := f.request(http.MethodPost, base+"/requests", f.writer, "request-key-1", body)
	if replay.Code != http.StatusAccepted || replay.Body.String() != created.Body.String() {
		t.Fatalf("request replay: %d %s", replay.Code, replay.Body.String())
	}
	changed := strings.Replace(body, strings.Repeat("a", 40), strings.Repeat("b", 40), 1)
	if got := f.request(http.MethodPost, base+"/requests", f.writer, "request-key-1", changed); got.Code != http.StatusConflict {
		t.Fatalf("changed request replay: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodPost, "/api/v1/projects/exec-project-two/reviews/requests", f.other, "other-project-key", body); got.Code != http.StatusConflict {
		t.Fatalf("other project unexpectedly enqueued unconfigured request: %d %s", got.Code, got.Body.String())
	}

	path := base + "/operations/" + receipt.ID
	read := f.request(http.MethodGet, path, f.reader, "", "")
	if read.Code != http.StatusOK {
		t.Fatalf("operation read: %d %s", read.Code, read.Body.String())
	}
	var operation Operation
	if err := json.Unmarshal(read.Body.Bytes(), &operation); err != nil || operation.ID != receipt.ID || operation.ProjectID != "exec-project-one" || operation.State != "queued" || operation.PullNumber != 2 {
		t.Fatalf("operation detail: %v %+v", err, operation)
	}
	if got := f.request(http.MethodGet, "/api/v1/projects/exec-project-two/reviews/operations/"+receipt.ID, f.other, "", ""); got.Code != http.StatusNotFound {
		t.Fatalf("cross-project operation read: %d %s", got.Code, got.Body.String())
	}
	if got := f.request(http.MethodGet, path, f.writer, "", ""); got.Code != http.StatusOK {
		t.Fatalf("writer read: %d %s", got.Code, got.Body.String())
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	if got := f.request(http.MethodGet, path, f.reader, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("revoked reader: %d %s", got.Code, got.Body.String())
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE project_id='exec-project-one'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("operations after replay/conflict: count=%d err=%v", n, err)
	}
}

func TestExecutionReadHTTPQueriesUseCommonValidationCodeWithoutEffects(t *testing.T) {
	f := newHTTPFixture(t)
	// No provider capability or driver is installed. A rejected query must not
	// reach domain lookup or any projection mutation behind these public reads.
	projections := 0
	e, err := New(f.store, nil, func(e *Engine) {
		e.publishAttempt = func(context.Context, *sql.Tx, Attempt) error {
			projections++
			return core.Fail("storage_unavailable", "Unexpected projection work.", 500)
		}
		e.publishAdjudication = func(context.Context, *sql.Tx, core.Scope, ReviewAdjudication) error {
			projections++
			return core.Fail("storage_unavailable", "Unexpected projection work.", 500)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	e.Register(mux)
	service := httptest.NewServer(mux)
	defer service.Close()
	client := service.Client()
	defer client.CloseIdleConnections()
	for _, route := range []struct {
		name, path    string
		missingStatus int
		missingCode   string
	}{
		{"configuration", "/api/v1/projects/exec-project-one/reviews/execution/configuration", http.StatusConflict, "reviews_unconfigured"},
		{"operation", "/api/v1/projects/exec-project-one/reviews/operations/review_operation_" + strings.Repeat("a", 32), http.StatusNotFound, "not_found"},
	} {
		for _, query := range []string{"?refresh=true", "?refresh=true&refresh=false", ""} {
			t.Run(route.name+query, func(t *testing.T) {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL+route.path+query, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+f.reader)
				request.Header.Set("X-FC-Instance", f.store.InstanceID())
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err = json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				expectedStatus, expectedCode := http.StatusBadRequest, "validation_failed"
				if query == "" {
					expectedStatus, expectedCode = route.missingStatus, route.missingCode
				}
				if response.StatusCode != expectedStatus || body.Error.Code != expectedCode {
					t.Fatalf("GET %s%s: status=%d code=%q; wanted %d/%s", route.path, query, response.StatusCode, body.Error.Code, expectedStatus, expectedCode)
				}
				assertNoExecutionMutation(t, f.store)
				for _, table := range []string{"review_execution_attempts", "review_execution_transitions", "review_execution_reservations"} {
					var count int
					if err = f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
						t.Fatalf("read changed %s: count=%d error=%v", table, count, err)
					}
				}
				if projections != 0 {
					t.Fatal("read performed projection work")
				}
			})
		}
	}
}
