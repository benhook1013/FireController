package mcpadapter_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const connectorOrigin = "https://connector.example.test"

type connectorTestTransport struct {
	next  http.RoundTripper
	token string
}

func (c connectorTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = "connector.example.test"
	r.Header.Set("Authorization", "Bearer "+c.token)
	return c.next.RoundTrip(r)
}

func connectorFixture(t *testing.T) (*mutationAPI, *httptest.Server, <-chan struct{}, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	f := mutationFixture(t)
	mux := f.handler.(*http.ServeMux)
	server.RegisterMessageStream(mux, f.store, t.Context())
	waitReady, waitDone, writeCommitted := make(chan struct{}, 16), make(chan struct{}, 16), make(chan struct{}, 1)
	var heldWrite atomic.Bool
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/message-events/stream") {
			defer func() { waitDone <- struct{}{} }()
			w = &concurrentMessageWaitReadyWriter{ResponseWriter: w, ready: waitReady}
		}
		mux.ServeHTTP(w, r)
		if r.Header.Get("Idempotency-Key") == "lost-response" && heldWrite.CompareAndSwap(false, true) {
			select {
			case writeCommitted <- struct{}{}:
				// Only the original write blocks. Signal once after commit, before
				// the typed client receives the response, and wait for cancellation.
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			default:
			}
		}
	}))
	s.Config.WriteTimeout = 500 * time.Millisecond
	handler, closeRemote, err := mcpadapter.NewRemoteHandler(t.Context(), f.store, mcpadapter.RemoteConfig{Origin: connectorOrigin, AllowedOrigins: []string{"https://client.example.test"}}, s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/mcp", handler)
	s.Start()
	t.Cleanup(s.Close)
	t.Cleanup(closeRemote)
	return f, s, waitReady, waitDone, writeCommitted
}

func connectorNative(t *testing.T, f *mutationAPI, method, path, token, worker, key string, body any, want int) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, want, w.Body.Bytes())
	}
	if want == http.StatusUnauthorized && !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer ") {
		t.Fatal("missing native bearer challenge")
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func connectorGrant(t *testing.T, f *mutationAPI, token, worker, key string, permissions []string, want int) map[string]any {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return connectorNative(t, f, http.MethodPost, "/api/v1/projects/"+f.project+"/credentials",
		f.admin, "", key, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "connector_worker_id": worker, "permissions": permissions}, want)
}

func connectorSDK(t *testing.T, s *httptest.Server, token, version string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "synthetic-connector", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	t.Cleanup(cancel)
	session, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.URL + "/mcp", DisableStandaloneSSE: true, HTTPClient: &http.Client{
			Transport: connectorTestTransport{next: s.Client().Transport, token: token},
		},
	}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	if session.InitializeResult().ProtocolVersion != version {
		t.Fatalf("negotiated %s, expected %s", session.InitializeResult().ProtocolVersion, version)
	}
	return session
}

func remoteToolCatalog(t *testing.T, session *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	var catalog []*mcp.Tool
	var cursor string
	for pages := 0; pages < 8; pages++ {
		out, err := session.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		catalog = append(catalog, out.Tools...)
		if out.NextCursor == "" {
			if pages == 0 {
				t.Fatal("tool discovery did not paginate")
			}
			return catalog
		}
		cursor = out.NextCursor
	}
	t.Fatal("catalog did not terminate")
	return nil
}

func TestRemoteConnectorSDKCatalogBindingAndNativeReplay(t *testing.T) {
	f, service, _, _, _ := connectorFixture(t)
	bearer := core.NewID("synthetic")
	path := "/api/v1/projects/" + f.project
	other := connectorNative(t, f, "POST", "/api/v1/projects", f.admin, "", "other-project",
		map[string]string{"name": "Other", "repository": "example/other"}, 200)["id"].(string)
	foreignWorker := connectorNative(t, f, "POST", "/api/v1/projects/"+other+"/workers", f.admin, "", "other-worker",
		map[string]string{"name": "Other worker"}, 200)["id"].(string)
	// Failure of the worker check leaves the digest available for the actual binding.
	connectorGrant(t, f, bearer, foreignWorker, "failed-grant", []string{"read"}, 404)
	original := connectorGrant(t, f, bearer, f.sender, "grant-connector", []string{"write", "read"}, 200)
	replay := connectorGrant(t, f, bearer, f.sender, "grant-connector", []string{"read", "write"}, 200)
	if !reflect.DeepEqual(original, replay) || original["connector_worker_id"] != f.sender {
		t.Fatal("binding was not retained in the grant receipt")
	}
	connectorGrant(t, f, bearer, f.recipient, "grant-connector", []string{"write", "read"}, 409)
	connectorGrant(t, f, bearer, f.recipient, "duplicate-digest", []string{"write", "read"}, 409)
	id := original["id"].(string)
	if _, err := f.store.DB().Exec(`UPDATE connector_credentials SET worker_id=? WHERE credential_id=?`, f.recipient, id); err == nil {
		t.Fatal("connector binding allowed update")
	}
	if _, err := f.store.DB().Exec(`DELETE FROM connector_credentials WHERE credential_id=?`, id); err == nil {
		t.Fatal("connector binding allowed deletion")
	}

	connectorNative(t, f, "GET", "/api/v1/instance", bearer, "", "", nil, 200)
	connectorNative(t, f, "GET", path, bearer, f.sender, "", nil, 200)
	connectorNative(t, f, "GET", path, bearer, f.recipient, "", nil, 403)
	connectorNative(t, f, "GET", path, bearer, "", "", nil, 403)
	connectorNative(t, f, "GET", "/api/v1/projects/"+other, bearer, f.sender, "", nil, 403)
	req := httptest.NewRequest("GET", path, nil)
	req.SetPathValue("project_id", f.project)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-FC-Worker", f.sender)
	scope, err := f.store.Scope(req, "read")
	if err != nil {
		t.Fatal(err)
	}
	scope.WorkerID = f.recipient
	if err := f.store.Read(t.Context(), scope, func(context.Context, *sql.Tx) error {
		t.Error("modified scope reached the protected read transaction")
		return nil
	}); err == nil {
		t.Fatal("read transaction accepted a different worker")
	}
	connectorNative(t, f, "POST", path+"/browser-session", bearer, f.sender, "",
		map[string]string{"session_id": "00000000000000000000000000000000"}, 403)

	remote := connectorSDK(t, service, bearer, "2026-07-28")
	legacy := connectorSDK(t, service, bearer, "2025-11-25")
	credFile := filepath.Join(t.TempDir(), "connector.token")
	if err := os.WriteFile(credFile, []byte(bearer+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stdio := func(project, worker string) *mcp.ClientSession {
		ctx := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: project, WorkerID: worker}
		return connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": ctx}, credFile), "chosen"), "")
	}
	bound := stdio(f.project, f.sender)
	if !reflect.DeepEqual(remoteToolCatalog(t, remote), remoteToolCatalog(t, bound)) {
		t.Fatal("Streamable HTTP and stdio catalogs differ")
	}
	var identity profile.Context
	decodeResult(t, call(t, legacy, "context", nil), &identity)
	if identity.InstanceID != f.store.InstanceID() || identity.ProjectID != f.project || identity.WorkerID != f.sender || identity.ServerURL != connectorOrigin {
		t.Fatal("incorrect server-derived binding")
	}
	errorResult(t, call(t, stdio(f.project, f.recipient), "jobs_list", nil), "forbidden", bearer)
	errorResult(t, call(t, stdio(other, foreignWorker), "jobs_list", nil), "forbidden", bearer)
	foreignJob := connectorNative(t, f, "POST", "/api/v1/projects/"+other+"/jobs", f.admin, foreignWorker, "foreign-job",
		map[string]string{"title": "Synthetic foreign job", "worker_id": foreignWorker}, 201)["id"].(string)
	errorResult(t, call(t, remote, "job_read", map[string]any{"job_id": foreignJob}), "not_found", bearer)

	args := map[string]any{"title": "Synthetic guarded create", "worker_id": f.sender, "idempotency_key": "create-connector-job"}
	var created, same client.JobReceipt
	decodeResult(t, call(t, remote, "jobs_create", args), &created)
	decodeResult(t, call(t, bound, "jobs_create", args), &same)
	if !reflect.DeepEqual(created, same) {
		t.Fatal("connector typed write did not share same-key replay with stdio")
	}
	read := map[string]any{"job_id": created.ID}
	if !reflect.DeepEqual(call(t, remote, "job_read", read).StructuredContent, call(t, bound, "job_read", read).StructuredContent) {
		t.Fatal("bound reads diverge")
	}

	readOnly := core.NewID("synthetic")
	connectorGrant(t, f, readOnly, f.sender, "grant-read-only", []string{"read"}, 200)
	errorResult(t, call(t, connectorSDK(t, service, readOnly, "2026-07-28"), "jobs_create", args), "forbidden", readOnly)

	connectorNative(t, f, "DELETE", path+"/credentials/"+id, f.admin, "", "revoke-connector", map[string]any{}, 200)
	connectorNative(t, f, "GET", path, bearer, f.sender, "", nil, 401)
}

func TestRemoteConnectorHTTPAdmission(t *testing.T) {
	f, service, _, _, _ := connectorFixture(t)
	bearer := core.NewID("synthetic")
	connectorGrant(t, f, bearer, f.sender, "grant-admission", []string{"read"}, 200)
	initialize := `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"clientInfo":{"name":"fixture","version":"1"},"protocolVersion":"2025-11-25","capabilities":{}}}`
	request := func(method, host, origin, token, body string, modify func(*http.Request), want int) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, service.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if modify != nil {
			modify(r)
		}
		resp, err := service.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want || strings.Contains(string(out), bearer) {
			t.Fatalf("HTTP status=%d want=%d, or exposed credential", resp.StatusCode, want)
		}
		if want == 401 && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer ") {
			t.Fatal("missing bearer challenge")
		}
		if want == 405 && resp.Header.Get("Allow") != "POST" {
			t.Fatal("missing permitted method")
		}
	}
	request("POST", "other.example.test", "", bearer, initialize, nil, 403)
	request("POST", "connector.example.test", "https://other.example.test", bearer, initialize, nil, 403)
	request("POST", "connector.example.test", "https://client.example.test", bearer, initialize, nil, 200)
	request("POST", "connector.example.test", "", "", initialize, nil, 401)
	request("POST", "connector.example.test", "", token, initialize, nil, 401)
	request("POST", "connector.example.test", "", f.admin, initialize, nil, 401)
	request("POST", "connector.example.test", "", bearer, initialize, func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+bearer) }, 401)
	request("POST", "connector.example.test", "", "", initialize, func(r *http.Request) {
		r.Header.Set("X-FC-Browser-Session", "00000000000000000000000000000000")
	}, 400)
	request("POST", "connector.example.test", "", bearer, initialize, func(r *http.Request) {
		r.Header.Set("X-FC-Worker", f.recipient)
	}, 400)
	request("POST", "connector.example.test", "", bearer, initialize, func(r *http.Request) {
		r.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		r.Header.Set("Mcp-Method", bearer)
	}, 400)
	request("GET", "connector.example.test", "", bearer, "", nil, 405)
	request("DELETE", "connector.example.test", "", bearer, "", nil, 405)
	request("POST", "connector.example.test", "", bearer, strings.Repeat(" ", client.ReviewBatchMaxBytes+17<<10), func(r *http.Request) {
		r.ContentLength = -1
	}, 413)
	connectorNative(t, f, "POST", "/api/v1/projects/"+f.project+"/workers/"+f.sender+"/retire",
		f.admin, "", "retire-connector-worker", map[string]any{}, 200)
	request("POST", "connector.example.test", "", bearer, initialize, nil, 401)
	connectorNative(t, f, "GET", "/api/v1/projects/"+f.project, bearer, f.sender, "", nil, 401)
}

func waitConnectorSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(4 * time.Second):
		t.Fatal("connector operation did not reach its expected boundary")
	}
}

func TestRemoteConnectorWaitDowngradeCancellationAndUncertainReplay(t *testing.T) {
	f, service, ready, detached, committed := connectorFixture(t)
	bearer := core.NewID("synthetic")
	grant := connectorGrant(t, f, bearer, f.sender, "grant-waiter", []string{"read", "write"}, 200)
	path := "/api/v1/projects/" + f.project
	cs := connectorSDK(t, service, bearer, "2025-11-25")
	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, cs, "inbox_snapshot", nil), &snapshot)

	wait := func(ctx context.Context) <-chan *mcp.CallToolResult {
		result := make(chan *mcp.CallToolResult, 1)
		go func() {
			out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{
				"cursor": snapshot.EventCursor, "wait_seconds": 5,
			}})
			if err != nil && ctx.Err() == nil {
				t.Error(err)
			}
			result <- out
		}()
		waitConnectorSignal(t, ready)
		return result
	}

	ctx, cancel := context.WithCancel(t.Context())
	canceled := wait(ctx)
	cancel()
	waitConnectorSignal(t, detached)
	if out := <-canceled; out != nil {
		t.Fatal("canceled wait returned a response")
	}
	waiting := wait(t.Context())
	// Wait for longer than the daemon's ordinary whole-response deadline.
	time.Sleep(600 * time.Millisecond)
	decodeResult(t, call(t, cs, "inbox_snapshot", nil), &snapshot)
	connectorNative(t, f, "PUT", path+"/credentials/"+grant["id"].(string), f.admin, "", "downgrade-waiter",
		map[string]any{"expected_permissions": []string{"read", "write"}, "permissions": []string{"read"}}, 200)
	connectorNative(t, f, "POST", path+"/messages", f.admin, f.recipient, "deliver-after-downgrade",
		map[string]string{"recipient_worker_id": f.sender, "body": "Synthetic retained message"}, 201)
	select {
	case out := <-waiting:
		errorResult(t, out, "connector_access_changed", bearer)
	case <-time.After(4 * time.Second):
		t.Fatal("downgraded wait did not complete")
	}
	waitConnectorSignal(t, detached)

	connectorNative(t, f, "PUT", path+"/credentials/"+grant["id"].(string), f.admin, "", "restore-waiter-write",
		map[string]any{"expected_permissions": []string{"read"}, "permissions": []string{"read", "write"}}, 200)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	params := &mcp.CallToolParams{Name: "jobs_create", Arguments: map[string]any{
		"title": "Synthetic lost response", "worker_id": f.sender, "idempotency_key": "lost-response",
	}}
	lost := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, params)
		lost <- err
	}()
	waitConnectorSignal(t, committed)
	cancel()
	if err := <-lost; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected lost response from canceled request: %v", err)
	}
	var receipt client.JobReceipt
	decodeResult(t, call(t, cs, params.Name, params.Arguments), &receipt)
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM jobs WHERE project_id=? AND title=? AND worker_id=?`, f.project, "Synthetic lost response", f.sender).Scan(&count); err != nil || count != 1 {
		t.Fatalf("guarded replay duplicated or lost the job: %d %v", count, err)
	}
}
