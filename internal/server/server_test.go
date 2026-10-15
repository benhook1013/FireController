package server_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

type fixture struct {
	store      *core.Store
	handler    http.Handler
	admin, dir string
	calls      atomic.Int64
	offline    atomic.Bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: filepath.Join(t.TempDir(), "private")}
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	data, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin = strings.TrimSpace(string(data))
	f.handler = server.New(f.store, server.WithRepositoryResolver(func(ctx context.Context, repository string) (server.RepositoryIdentity, error) {
		f.calls.Add(1)
		if f.offline.Load() {
			return server.RepositoryIdentity{}, errors.New("private provider failure")
		}
		id := int64(1)
		if repository == "owner/two" {
			id = 2
		}
		return server.RepositoryIdentity{ID: id, FullName: repository}, nil
	}))
	return f
}

func request(handler http.Handler, method, path, token, key, worker, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func require(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d response=%s", w.Code, status, w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *fixture) project(t *testing.T, repository string) string {
	t.Helper()
	value := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, core.NewID("key"), "", `{"name":"project","repository":"`+repository+`"}`), 200)
	return value["id"].(string)
}
func (f *fixture) worker(t *testing.T, project, name string) string {
	t.Helper()
	value := require(t, request(f.handler, "POST", "/api/v1/projects/"+project+"/workers", f.admin, core.NewID("key"), "", `{"name":"`+name+`"}`), 200)
	return value["id"].(string)
}
func (f *fixture) grant(t *testing.T, project, token string, write bool) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	permissions := `["read"]`
	if write {
		permissions = `["write","read"]`
	}
	value := require(t, request(f.handler, "POST", "/api/v1/projects/"+project+"/credentials", f.admin, core.NewID("key"), "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":`+permissions+`}`), 200)
	return value["id"].(string)
}
func count(t *testing.T, store *core.Store, table string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBootstrapRestartAndUnknownSchema(t *testing.T) {
	f := setup(t)
	instance := f.store.InstanceID()
	info, err := os.Stat(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("bootstrap credential permissions: %v %v", info, err)
	}
	require(t, request(f.handler, "GET", "/healthz", "", "", "", ""), 200)
	require(t, request(f.handler, "GET", "/readyz", "", "", "", ""), 200)
	require(t, request(f.handler, "GET", "/api/v1/instance", "", "", "", ""), 401)
	value := require(t, request(f.handler, "GET", "/api/v1/instance", f.admin, "", "", ""), 200)
	if value["instance_id"] != instance {
		t.Fatal("instance mismatch")
	}
	project := f.project(t, "owner/one")
	f.worker(t, project, "Builder")
	scopeRequest := httptest.NewRequest("GET", "/", nil)
	scopeRequest.SetPathValue("project_id", project)
	scopeRequest.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.Scope(scopeRequest, "read")
	if err != nil {
		t.Fatal(err)
	}
	cursor := f.store.Cursor(scope, "restart-proof", "after-id")
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.store.InstanceID() != instance || count(t, f.store, "projects") != 1 || count(t, f.store, "workers") != 1 || count(t, f.store, "receipts") != 2 || count(t, f.store, "events") != 2 {
		t.Fatal("durable state or identity lost")
	}
	if after, err := f.store.ParseCursor(scope, "restart-proof", cursor); err != nil || after != "after-id" {
		t.Fatal("restart lost cursor signing identity")
	}
	if _, err = f.store.DB().Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	unexpected, err := core.Open(context.Background(), f.dir)
	if unexpected != nil {
		_ = unexpected.Close()
		t.Fatal("newer schema accepted")
	}
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "schema_unsupported" {
		t.Fatalf("schema refusal: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("unsupported schema database changed")
	}
}

func TestProjectCreationAuthenticatesAndReplaysBeforeProvider(t *testing.T) {
	f := setup(t)
	body := `{"name":"project","repository":"owner/one"}`
	require(t, request(f.handler, "POST", "/api/v1/projects", "", "first", "", body), 401)
	if f.calls.Load() != 0 {
		t.Fatal("unauthenticated provider call")
	}
	f.offline.Store(true)
	require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "first", "", body), 503)
	if count(t, f.store, "projects") != 0 || count(t, f.store, "events") != 0 || count(t, f.store, "receipts") != 0 {
		t.Fatal("failed verification wrote state")
	}
	f.offline.Store(false)
	first := request(f.handler, "POST", "/api/v1/projects", f.admin, "first", "", body)
	require(t, first, 200)
	calls := f.calls.Load()
	f.offline.Store(true)
	replay := request(f.handler, "POST", "/api/v1/projects", f.admin, "first", "", body)
	require(t, replay, 200)
	if first.Body.String() != replay.Body.String() || f.calls.Load() != calls {
		t.Fatal("replay changed receipt or called provider")
	}
	changed := request(f.handler, "POST", "/api/v1/projects", f.admin, "first", "", `{"name":"changed","repository":"owner/one"}`)
	require(t, changed, 409)
	if f.calls.Load() != calls || count(t, f.store, "projects") != 1 || count(t, f.store, "events") != 1 || count(t, f.store, "receipts") != 1 {
		t.Fatal("conflict called provider or changed state")
	}
	f.offline.Store(false)
	duplicate := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "alias", "", `{"name":"alias","repository":"owner/renamed"}`), 409)
	if duplicate["error"].(map[string]any)["code"] != "repository_already_configured" {
		t.Fatal("stable repository identity not enforced")
	}
	require(t, request(server.New(f.store), "POST", "/api/v1/projects", f.admin, "missing", "", body), 503)
}

func TestProjectCreationPreservesOnlyTypedRepositoryNotFound(t *testing.T) {
	f := setup(t)
	const privateDetail = "private-resolver-detail"
	missing := server.New(f.store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{}, &core.Error{Code: "repository_not_found", Message: privateDetail, Status: http.StatusNotFound, Details: map[string]string{"secret": "must-not-escape"}}
	}))
	response := request(missing, "POST", "/api/v1/projects", f.admin, "missing-project", "", `{"name":"missing","repository":"owner/missing"}`)
	value := require(t, response, http.StatusNotFound)
	if value["error"].(map[string]any)["code"] != "repository_not_found" || strings.Contains(response.Body.String(), privateDetail) || strings.Contains(response.Body.String(), "must-not-escape") {
		t.Fatalf("unsafe or incorrect not-found response: %s", response.Body.String())
	}
	if count(t, f.store, "projects") != 0 || count(t, f.store, "events") != 0 || count(t, f.store, "receipts") != 0 {
		t.Fatal("not-found resolver wrote project state")
	}
	transient := server.New(f.store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{}, &core.Error{Code: "provider_rate_limited", Message: privateDetail, Status: http.StatusTooManyRequests}
	}))
	response = request(transient, "POST", "/api/v1/projects", f.admin, "transient-project", "", `{"name":"retry","repository":"owner/retry"}`)
	value = require(t, response, http.StatusServiceUnavailable)
	if value["error"].(map[string]any)["code"] != "provider_unavailable" || strings.Contains(response.Body.String(), privateDetail) {
		t.Fatalf("transient resolver failure was not safely normalized: %s", response.Body.String())
	}
	if count(t, f.store, "projects") != 0 || count(t, f.store, "events") != 0 || count(t, f.store, "receipts") != 0 {
		t.Fatal("transient resolver failure wrote project state")
	}
}

func TestProjectClientsWorkersAttributionAndRevocation(t *testing.T) {
	f := setup(t)
	p, q := f.project(t, "owner/one"), f.project(t, "owner/two")
	workerP, workerQ := f.worker(t, p, "Builder"), f.worker(t, q, "Builder")
	readToken, writeToken := "synthetic-read-credential-00000000000", "synthetic-write-credential-0000000000"
	f.grant(t, p, readToken, false)
	credential := f.grant(t, p, writeToken, true)
	calls := f.calls.Load()
	path := "/api/v1/projects/" + p + "/workers"
	require(t, request(f.handler, "GET", path, readToken, "", "", ""), 200)
	require(t, request(f.handler, "GET", "/api/v1/projects/"+q+"/workers", readToken, "", "", ""), 403)
	require(t, request(f.handler, "GET", path+"/"+workerQ, readToken, "", "", ""), 404)
	require(t, request(f.handler, "GET", path, writeToken, "", workerQ, ""), 404)
	require(t, request(f.handler, "POST", path, readToken, "read-only", "", `{"name":"other"}`), 403)
	require(t, request(f.handler, "POST", "/api/v1/projects", writeToken, "admin-denied", "", `{"name":"third","repository":"owner/third"}`), 403)
	require(t, request(f.handler, "GET", "/api/v1/instance", writeToken, "", "", ""), 200)
	first := request(f.handler, "POST", path, writeToken, "worker-create", "", `{"name":"other"}`)
	require(t, first, 200)
	changedActor := request(f.handler, "POST", path, writeToken, "worker-create", workerP, `{"name":"other"}`)
	require(t, changedActor, 409)
	beforeEvents := count(t, f.store, "events")
	require(t, request(f.handler, "DELETE", "/api/v1/projects/"+p+"/credentials/"+credential, f.admin, "revoke", "", ""), 200)
	require(t, request(f.handler, "POST", path, writeToken, "worker-create", "", `{"name":"other"}`), 401)
	require(t, request(f.handler, "GET", path, writeToken, "", "", ""), 401)
	if count(t, f.store, "events") != beforeEvents+1 || f.calls.Load() != calls {
		t.Fatal("revoked replay wrote state or ordinary operation called provider")
	}
	for _, token := range []string{f.admin, readToken, writeToken} {
		if strings.Contains(first.Body.String(), token) {
			t.Fatal("receipt exposed credential")
		}
	}
}

func TestStrictRequestsAndScopedPagination(t *testing.T) {
	f := setup(t)
	p, q := f.project(t, "owner/one"), f.project(t, "owner/two")
	path := "/api/v1/projects/" + p + "/workers"
	for _, body := range []string{`{"name":"one","name":"two"}`, `{"Name":"one"}`, `{"name":"one","secret":"PRIVATE_BODY"}`, `{"name":"one"}{}`, `{"name":"bad-\ud800"}`, `{"name":"bad-\udfff"}`, "{\"name\":\"bad-" + string([]byte{255}) + "\"}", `null`, strings.Repeat(" ", (256<<10)+1)} {
		w := request(f.handler, "POST", path, f.admin, core.NewID("key"), "", body)
		require(t, w, 400)
		if strings.Contains(w.Body.String(), "PRIVATE_BODY") || strings.Contains(w.Body.String(), f.admin) {
			t.Fatal("diagnostic exposed request or credential")
		}
	}
	if count(t, f.store, "workers") != 0 {
		t.Fatal("invalid request wrote worker")
	}
	f.worker(t, p, "one")
	f.worker(t, p, "two")
	value := require(t, request(f.handler, "GET", path+"?limit=1", f.admin, "", "", ""), 200)
	cursor := value["cursor"].(string)
	if value["has_more"] != true || cursor == "" {
		t.Fatal("missing bounded page cursor")
	}
	require(t, request(f.handler, "GET", "/api/v1/projects/"+q+"/workers?cursor="+cursor, f.admin, "", "", ""), 400)
	token := "synthetic-page-credential-00000000000"
	f.grant(t, p, token, false)
	require(t, request(f.handler, "GET", path+"?cursor="+cursor, token, "", "", ""), 400)
	require(t, request(f.handler, "GET", path+"?limit=101", f.admin, "", "", ""), 400)
	require(t, request(f.handler, "GET", path+"?limit=1&limit=2", f.admin, "", "", ""), 400)
	require(t, request(f.handler, "GET", path+"?cursor="+cursor, f.admin, "", "", ""), 200)
}

func TestProjectAndWorkerListsShareExactPaginationContract(t *testing.T) {
	f := setup(t)
	p, q := f.project(t, "owner/one"), f.project(t, "owner/two")
	w1, w2 := f.worker(t, p, "one"), f.worker(t, p, "two")
	f.worker(t, q, "other-project")
	calls := f.calls.Load()
	events, receipts := count(t, f.store, "events"), count(t, f.store, "receipts")
	for _, tc := range []struct {
		name, path string
		ids        []string
	}{
		{"projects", "/api/v1/projects", []string{p, q}},
		{"workers", "/api/v1/projects/" + p + "/workers", []string{w1, w2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := slices.Clone(tc.ids)
			slices.Sort(want)
			cursor := ""
			for page := range 2 {
				path := tc.path + "?limit=1"
				if cursor != "" {
					path += "&cursor=" + cursor
				}
				value := require(t, request(f.handler, "GET", path, f.admin, "", "", ""), 200)
				if (page == 0 && len(value) != 3) || (page == 1 && len(value) != 2) {
					t.Fatalf("pagination response has extra or missing fields: %v", value)
				}
				items, ok := value["items"].([]any)
				if !ok || len(items) != 1 {
					t.Fatalf("expected one item on page %d: %v", page, value)
				}
				item := items[0].(map[string]any)
				if item["id"] != want[page] {
					t.Fatalf("item skipped, duplicated or out of order: %v", item)
				}
				if tc.name == "workers" && (item["project_id"] != p || (item["name"] != "one" && item["name"] != "two")) {
					t.Fatalf("worker list escaped selected project: %v", item)
				}
				if tc.name == "projects" && (item["name"] != "project" || (item["repository"] != "owner/one" && item["repository"] != "owner/two")) {
					t.Fatalf("project item lost metadata: %v", item)
				}
				cursor, ok = value["cursor"].(string)
				if (page == 0 && !ok) || (page == 1 && value["cursor"] != nil) {
					t.Fatal("cursor must be present only when another page is available")
				}
				if value["has_more"] != (page == 0) || (page == 0 && cursor == "") || (page == 1 && cursor != "") {
					t.Fatalf("incorrect continuation on page %d: %v", page, value)
				}
			}
			for _, query := range []string{
				"status=active", "worker_id=unknown", "unknown=PRIVATE_FILTER", "limit=1&limit=2", "limit=1&limit=1",
				"cursor=&cursor=", "cursor=first&cursor=second", "cursor=&%63ursor=", "limit=1&%6cimit=2",
				"cursor=%zz", "limit=1;cursor=", "cursor=" + strings.Repeat("a", 4097), "cursor=" + strings.Repeat("a", (16<<10)+1),
			} {
				value := require(t, request(f.handler, "GET", tc.path+"?"+query, f.admin, "", "", ""), 400)
				if value["error"].(map[string]any)["code"] != "validation_failed" {
					t.Fatalf("unexpected query error: %v", value)
				}
			}
		})
	}
	if f.calls.Load() != calls || count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts {
		t.Fatal("list/pagination/query failures invoked provider or changed durable state")
	}
}

func TestConcurrentKeysAndRepositoryBindingAreAtomic(t *testing.T) {
	f := setup(t)
	const contenders = 8
	var group sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, contenders)
	for i := 0; i < contenders; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- request(f.handler, "POST", "/api/v1/projects", f.admin, core.NewID("key"), "", `{"name":"same","repository":"owner/one"}`)
		}()
	}
	group.Wait()
	close(results)
	winners := 0
	project := ""
	for result := range results {
		if result.Code == 200 {
			winners++
			project = require(t, result, 200)["id"].(string)
		} else {
			require(t, result, 409)
		}
	}
	if winners != 1 || count(t, f.store, "projects") != 1 || count(t, f.store, "events") != 1 || count(t, f.store, "receipts") != 1 {
		t.Fatal("repository race created partial or duplicate state")
	}
	results = make(chan *httptest.ResponseRecorder, contenders)
	for i := 0; i < contenders; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- request(f.handler, "POST", "/api/v1/projects/"+project+"/workers", f.admin, "shared-key", "", `{"name":"Builder"}`)
		}()
	}
	group.Wait()
	close(results)
	first := ""
	for result := range results {
		require(t, result, 200)
		if first == "" {
			first = result.Body.String()
		} else if result.Body.String() != first {
			t.Fatal("concurrent key receipt differed")
		}
	}
	if count(t, f.store, "workers") != 1 || count(t, f.store, "events") != 2 || count(t, f.store, "receipts") != 2 {
		t.Fatal("same-key race created duplicate domain state")
	}
}

func TestStartupMigrationAndMutationFailuresRollback(t *testing.T) {
	f := setup(t)
	if err := f.store.EnsureSchema(context.Background(), "startup_probe", []string{"CREATE TABLE startup_probe(id INTEGER)", "INVALID DDL"}); err == nil {
		t.Fatal("failed startup schema succeeded")
	}
	var exists int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='startup_probe'").Scan(&exists); err != nil || exists != 0 {
		t.Fatal("partial startup schema")
	}
	p := f.project(t, "owner/one")
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", p)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.Scope(r, "write")
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, beforeReceipts := count(t, f.store, "events"), count(t, f.store, "receipts")
	for _, encodeFailure := range []bool{false, true} {
		_, err = f.store.Mutate(context.Background(), scope, core.Mutation{Operation: "proof.fail", Target: "workers", Key: core.NewID("key"), Request: struct{}{}}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO workers(project_id,id,name) VALUES(?,?,?)", p, core.NewID("worker"), "rollback"); err != nil {
				return nil, err
			}
			if err := core.AppendEvent(ctx, tx, scope, "proof.failed", "rollback", 1, struct{}{}); err != nil {
				return nil, err
			}
			if encodeFailure {
				return make(chan int), nil
			}
			return nil, core.Fail("validation_failed", "Synthetic failure.", 400)
		})
		if err == nil {
			t.Fatal("failed mutation succeeded")
		}
	}
	if count(t, f.store, "workers") != 0 || count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
		t.Fatal("failure left partial state/event/receipt")
	}
	_, err = f.store.Mutate(context.Background(), core.Scope{PrincipalID: scope.PrincipalID, ProjectID: p}, core.Mutation{Operation: "proof.forged", Target: "workers", Key: "forged", Request: struct{}{}}, func(context.Context, *sql.Tx) (any, error) { t.Fatal("forged scope callback"); return nil, nil })
	if err == nil {
		t.Fatal("forged unauthenticated scope accepted")
	}
}

func TestBusyWriteFailsBoundedlyAndRetrySucceeds(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	conn, err := f.store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	path := "/api/v1/projects/" + p + "/workers"
	start := time.Now()
	w := request(f.handler, "POST", path, f.admin, "busy-key", "", `{"name":"Builder"}`)
	value := require(t, w, 503)
	if value["error"].(map[string]any)["code"] != "overloaded" || time.Since(start) > time.Second {
		t.Fatal("busy write did not fail promptly")
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "POST", path, f.admin, "busy-key", "", `{"name":"Builder"}`), 200)
	if count(t, f.store, "workers") != 1 || count(t, f.store, "events") != 2 || count(t, f.store, "receipts") != 2 {
		t.Fatal("busy failure persisted partial state")
	}
}

func TestAdmissionCancellationAndCursorFilterBinding(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", p)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.Scope(r, "write")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.store.Mutate(context.Background(), scope, core.Mutation{Operation: "proof.hold", Target: "probe", Key: "hold", Request: struct{}{}}, func(context.Context, *sql.Tx) (any, error) { close(entered); <-release; return struct{}{}, nil })
		done <- err
	}()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("holder deadline")
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("holder did not enter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = f.store.Mutate(ctx, scope, core.Mutation{Operation: "proof.cancel", Target: "probe", Key: "cancel", Request: struct{}{}}, func(context.Context, *sql.Tx) (any, error) {
		t.Error("cancelled admission called callback")
		return nil, nil
	})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" || time.Since(start) > time.Second {
		t.Fatalf("admission cancellation: %v", err)
	}
	cursor := f.store.Cursor(scope, "proof.cursor", "after-id")
	if _, err = f.store.ParseCursor(scope, "different-filter", cursor); err == nil {
		t.Fatal("filter-unbound cursor")
	}
	if value, err := f.store.ParseCursor(scope, "proof.cursor", cursor); err != nil || value != "after-id" {
		t.Fatal("cursor did not round trip")
	}
}

func TestExpectedInstanceGuardsReadsMutationsAndProviderWork(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	worker := f.worker(t, p, "Builder")
	token := strings.Repeat("guard-token-", 4)
	f.grant(t, p, token, true)
	calls := f.calls.Load()
	events, receipts := count(t, f.store, "events"), count(t, f.store, "receipts")
	call := func(method, path, credential, body string, expected ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Idempotency-Key", "guard-key")
		for _, value := range expected {
			r.Header.Add("X-FC-Instance", value)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	paths := []struct{ method, path, credential, body string }{
		{"GET", "/api/v1/instance", token, ""},
		{"GET", "/api/v1/projects/" + p + "/workers/" + worker, token, ""},
		{"POST", "/api/v1/projects/" + p + "/workers", token, `{"name":"Unwanted"}`},
		{"POST", "/api/v1/projects", f.admin, `{"name":"Unwanted","repository":"owner/two"}`},
	}
	for _, tc := range paths {
		bad := require(t, call(tc.method, tc.path, tc.credential, tc.body, "instance-replaced"), 409)
		if bad["error"].(map[string]any)["code"] != "instance_mismatch" {
			t.Fatal("mismatch code missing")
		}
		for _, expected := range [][]string{{""}, {" padded "}, {"malformed instance"}, {"malformed\tinstance"}, {"malformed\u00a0instance"}, {string([]byte{255})}, {strings.Repeat("x", 129)}, {f.store.InstanceID(), f.store.InstanceID()}} {
			require(t, call(tc.method, tc.path, tc.credential, tc.body, expected...), 400)
		}
	}
	require(t, call("GET", "/api/v1/instance", "wrong-credential", "", "instance-replaced"), 401)
	require(t, call("GET", "/api/v1/instance", token, "", f.store.InstanceID()), 200)
	require(t, call("GET", "/api/v1/projects/"+p+"/workers/"+worker, token, "", f.store.InstanceID()), 200)
	// Existing bootstrap/raw API calls without a bound expected-instance header remain valid.
	require(t, call("GET", "/api/v1/instance", token, ""), 200)
	if f.calls.Load() != calls || count(t, f.store, "workers") != 1 || count(t, f.store, "projects") != 1 || count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts {
		t.Fatal("guard denial changed domain/provider state")
	}
}
func TestWriteDeadlineInterruptsStagedSQLiteAndKeepsKeyRetryable(t *testing.T) {
	// Run the expensive statement only in an externally bounded child. Losing
	// propagation to SQLite must fail this proof, never hang the parent suite.
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--write-deadline-child" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, executable, "-test.run=^TestWriteDeadlineInterruptsStagedSQLiteAndKeepsKeyRetryable$", "-test.timeout=8s", "--", "--write-deadline-child")
		child.WaitDelay = time.Second
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("bounded write deadline child failed: %v: %s", err, output)
		}
		return
	}
	f := setup(t)
	project := f.project(t, "owner/one")
	calls := f.calls.Load()
	for _, shortParent := range []bool{false, true} {
		// workers.create stages its worker first, then AppendEvent executes this
		// trigger. Both staged domain and event work must disappear on expiry.
		domain := core.NewID("deadline")
		createTrigger := []string{`CREATE TRIGGER deadline_probe AFTER INSERT ON events WHEN NEW.event_type='workers.created' BEGIN
SELECT sum(x) FROM (WITH RECURSIVE counter(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM counter WHERE x<1000000000) SELECT x FROM counter);
END`}
		if err := f.store.EnsureSchema(context.Background(), domain, createTrigger); err != nil {
			t.Fatal(err)
		}
		beforeWorkers, beforeEvents, beforeReceipts := count(t, f.store, "workers"), count(t, f.store, "events"), count(t, f.store, "receipts")
		key := core.NewID("key")
		body := `{"name":"` + key + `"}`
		path := "/api/v1/projects/" + project + "/workers"
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("Idempotency-Key", key)
		ctx := context.Background()
		cancel := func() {}
		if shortParent {
			ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
		}
		r = r.WithContext(ctx)
		response := httptest.NewRecorder()
		start := time.Now()
		f.handler.ServeHTTP(response, r)
		elapsed := time.Since(start)
		cancel()
		value := require(t, response, 503)
		if value["error"].(map[string]any)["code"] != "overloaded" || elapsed > 4*time.Second || shortParent && elapsed > time.Second {
			t.Fatalf("write deadline did not return a prompt safe overload: %s", response.Body.String())
		}
		if !shortParent && elapsed < time.Second {
			t.Fatal("default deadline proof did not exercise the two-second write budget")
		}
		if count(t, f.store, "workers") != beforeWorkers || count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
			t.Fatal("expired write left domain state, event or receipt")
		}
		if err := f.store.EnsureSchema(context.Background(), domain, createTrigger, []string{"DROP TRIGGER deadline_probe"}); err != nil {
			t.Fatal(err)
		}
		first := request(f.handler, "POST", path, f.admin, key, "", body)
		require(t, first, 200)
		replayed := request(f.handler, "POST", path, f.admin, key, "", body)
		require(t, replayed, 200)
		if first.Body.String() != replayed.Body.String() || count(t, f.store, "workers") != beforeWorkers+1 || count(t, f.store, "events") != beforeEvents+1 || count(t, f.store, "receipts") != beforeReceipts+1 {
			t.Fatal("same-key retry after expiry duplicated or lost atomic state")
		}
	}
	if f.calls.Load() != calls {
		t.Fatal("deadline/retry proof performed provider work")
	}
}

func TestCanceledCallbacksAndRealStorageFailureStayDistinct(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.Scope(r, "write")
	if err != nil {
		t.Fatal(err)
	}
	mutation := core.Mutation{Operation: "proof.deadline", Target: "workers", Key: "cancel-before", Request: struct{}{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.store.Mutate(ctx, scope, mutation, func(context.Context, *sql.Tx) (any, error) {
		t.Fatal("pre-canceled mutation called callback")
		return nil, nil
	})
	assert := func(err error, code string, status int) {
		t.Helper()
		response := httptest.NewRecorder()
		core.WriteError(response, err)
		value := require(t, response, status)
		if value["error"].(map[string]any)["code"] != code || strings.Contains(response.Body.String(), "PRIVATE_SQL") {
			t.Fatalf("unsafe/wrong failure class: %s", response.Body.String())
		}
	}
	assert(err, "overloaded", 503)
	for _, returnError := range []bool{false, true} {
		beforeWorkers, beforeEvents, beforeReceipts := count(t, f.store, "workers"), count(t, f.store, "events"), count(t, f.store, "receipts")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		mutation.Key = core.NewID("key")
		_, err = f.store.Mutate(ctx, scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO workers(project_id,id,name) VALUES(?,?,?)", project, core.NewID("worker"), mutation.Key); err != nil {
				return nil, err
			}
			if err := core.AppendEvent(ctx, tx, scope, "proof.staged", "worker", 1, struct{}{}); err != nil {
				return nil, err
			}
			<-ctx.Done()
			if returnError {
				return nil, sql.ErrTxDone
			}
			return struct{}{}, nil
		})
		cancel()
		assert(err, "overloaded", 503)
		if count(t, f.store, "workers") != beforeWorkers || count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
			t.Fatal("callback expiry left partial state")
		}
		_, err = f.store.Mutate(context.Background(), scope, mutation, func(context.Context, *sql.Tx) (any, error) { return struct{}{}, nil })
		if err != nil {
			t.Fatalf("expired key could not retry: %v", err)
		}
	}
	mutation.Key = "real-storage-failure"
	_, err = f.store.Mutate(context.Background(), scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO PRIVATE_SQL_missing_table(id) VALUES(1)")
		return nil, err
	})
	assert(err, "storage_unavailable", 500)
	mutation.Key = "unexpected-completed-transaction"
	_, err = f.store.Mutate(context.Background(), scope, mutation, func(context.Context, *sql.Tx) (any, error) { return nil, sql.ErrTxDone })
	assert(err, "storage_unavailable", 500)
}
