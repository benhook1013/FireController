package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/server"
)

func historyCollectionDaemonSource(t *testing.T) activationCommandFixture {
	t.Helper()
	f := activationCommandFixture{dir: filepath.Join(t.TempDir(), "private")}
	store, err := core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.token, f.instance = strings.TrimSpace(string(token)), store.InstanceID()
	mux := server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 123, FullName: "synthetic/history-daemon"}, nil
	}))
	for _, route := range []struct {
		path, body string
		target     *string
	}{
		{"/api/v1/projects", `{"name":"Synthetic history daemon","repository":"synthetic/history-daemon"}`, &f.project},
		{"", `{"name":"collector"}`, &f.worker},
	} {
		path := route.path
		if path == "" {
			path = "/api/v1/projects/" + f.project + "/workers"
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(route.body))
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("X-FC-Instance", store.InstanceID())
		r.Header.Set("Idempotency-Key", core.NewID("fixture"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var response struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.ID == "" {
			t.Fatal("offline daemon seed failed", w.Code)
		}
		*route.target = response.ID
	}
	return f
}

func historyCollectionDaemonStart(t *testing.T, f activationCommandFixture, args ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stdout, stderr := &output{ready: make(chan struct{})}, &output{ready: make(chan struct{})}
	done := make(chan int, 1)
	flags := append([]string{"--data-dir", f.dir, "--listen", "127.0.0.1:0"}, args...)
	go func() { done <- run(ctx, flags, stdout, stderr) }()
	select {
	case <-stdout.ready:
	case code := <-done:
		cancel()
		t.Fatalf("history daemon exited during startup, code=%d", code)
	case <-time.After(8 * time.Second):
		cancel()
		t.Fatal("history daemon startup deadline")
	}
	var start map[string]string
	if json.Unmarshal([]byte(stdout.String()), &start) != nil || start["instance_id"] != f.instance || start["address"] == "" {
		cancel()
		t.Fatal("history daemon lost identity")
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case code := <-done:
			if code != 0 || stderr.String() != "" || strings.Contains(stdout.String(), f.token) {
				t.Fatalf("history daemon/owned worker failed clean shutdown, code=%d", code)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("history daemon/owned worker failed to join")
		}
	}
	t.Cleanup(stop)
	return "http://" + start["address"], stop
}

func historyCollectionDaemonCounts(t *testing.T, dir string) {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, "firecontroller.sqlite"))}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	u.RawQuery = url.Values{"mode": {"ro"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:history_collection'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("history schema was not installed independently of opt-in", err)
	}
	for _, table := range []string{"history_collections", "history_collection_pages", "history_collection_seen"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("startup or unavailable request invented collection work", table, err)
		}
	}
}

func TestHistoryCollectionDaemonDefaultDisabledAndEnabledEmptyQueueJoin(t *testing.T) {
	f := historyCollectionDaemonSource(t)
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			var args []string
			if enabled {
				tokenFile := filepath.Join(t.TempDir(), "selected.token")
				if err := os.WriteFile(tokenFile, []byte("synthetic-history-provider-token\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = []string{"--history-collection", "--github-token-file", tokenFile}
			}
			base, stop := historyCollectionDaemonStart(t, f, args...)
			status, _ := activationHTTP(t, base, "GET", "/readyz", f.token, f.instance, f.worker, nil)
			if status != 200 {
				t.Fatal("history mode prevented readiness", status)
			}
			path := "/api/v1/projects/" + f.project + "/history/collections"
			if !enabled {
				status, response := activationHTTP(t, base, "POST", path, f.token, f.instance, f.worker, map[string]any{"pull_numbers": []int{47}})
				if status != 409 || response["error"].(map[string]any)["code"] != "collection_unavailable" {
					t.Fatal("disabled history API missing or admitted work", status)
				}
			}
			status, _ = activationHTTP(t, base, "GET", path+"/history_collection_"+strings.Repeat("a", 32), f.token, f.instance, f.worker, nil)
			if status != 404 {
				t.Fatal("history authenticated read route missing", status)
			}
			stop()
			historyCollectionDaemonCounts(t, f.dir)
		})
	}
}

func TestHistoryCollectionDaemonRecoveryHoldIgnoresOptInAndCredentialCatalog(t *testing.T) {
	f := newActivationCommandFixture(t)
	var activated backup.ActivationSummary
	if json.Unmarshal(runBackupResult(t, f.args()...), &activated) != nil || !activated.ProvidersHeld {
		t.Fatal("fixture is not a genuine provider-held recovery")
	}
	before := activationWorkRows(t, f.dir)
	base, stop := activationStart(t, f, "--history-collection")
	status, response := activationHTTP(t, base, "POST", "/api/v1/projects/"+f.project+"/history/collections", f.token, f.instance, f.worker, map[string]any{"pull_numbers": []int{47}})
	if status != 409 || response["error"].(map[string]any)["code"] != "collection_unavailable" {
		t.Fatal("provider-held history admitted acquisition", status)
	}
	stop()
	if after := activationWorkRows(t, f.dir); !reflect.DeepEqual(before, after) {
		t.Fatal("provider-held boot recovered or rewrote native provider work")
	}
	historyCollectionDaemonCounts(t, f.dir)
}

func TestHistoryCollectionDaemonInvalidReaderRefusesBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "selected.token")
	if err := os.WriteFile(path, []byte("PRIVATE_INVALID_HISTORY_TOKEN\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "must-not-create")
	var out, diagnostics bytes.Buffer
	code := run(t.Context(), []string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--history-collection", "--github-token-file", path}, &out, &diagnostics)
	if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"configuration_invalid"`) || strings.Contains(diagnostics.String(), path) || strings.Contains(diagnostics.String(), "PRIVATE_INVALID_HISTORY_TOKEN") {
		t.Fatal("invalid enabled history reader was adopted or disclosed")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid enabled reader created persistent state")
	}
}

func TestHistoryCollectionDaemonStopsWhenAcquisitionLaneFails(t *testing.T) {
	f := historyCollectionDaemonSource(t)
	store, err := core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = historycollect.EnsureSchema(t.Context(), store); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	// Deliberately corrupt a queued local row in the stopped fixture. This is
	// failure injection into storage, not a manufactured acquisition success.
	// The worker rejects selection JSON before any provider read can begin.
	_, err = store.DB().Exec(`INSERT INTO history_collections(id,project_id,repository_id,actor_id,worker_id,reader_fingerprint,selection_json,state,revision,reason,position_json,pass_number,page_count,record_count,created_at,updated_at) VALUES(?,?,123,'invalid-fixture',?,?,'{}','queued',1,'',?,1,0,0,?,?)`, "history_collection_"+strings.Repeat("a", 32), f.project, f.worker, strings.Repeat("a", 64), `{"pull_index":0,"resource":"pull","page":1}`, "2026-10-17T00:00:00Z", "2026-10-17T00:00:00Z")
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var out, diagnostics bytes.Buffer
	code := run(ctx, []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0", "--history-collection"}, &out, &diagnostics)
	if code != 3 || ctx.Err() != nil || !strings.Contains(diagnostics.String(), `"code":"storage_unavailable"`) || strings.Contains(diagnostics.String(), f.token) {
		t.Fatal("daemon kept serving after its acquisition lane failed", code)
	}
}

// This provider is an enqueue-only fixture: acquisition is never started and
// no provider method has an implementation capable of external work.
type historyCollectionEnqueueOnly struct{ historycollect.Provider }

func TestHistoryCollectionDaemonShutdownDetachesPendingGET(t *testing.T) {
	f := historyCollectionDaemonSource(t)
	store, err := core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = historycollect.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	engine, err := historycollect.New(store, historyCollectionEnqueueOnly{}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	mux := server.New(store)
	engine.Register(mux)
	r := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/history/collections", strings.NewReader(`{"pull_numbers":[47]}`))
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.instance)
	r.Header.Set("X-FC-Worker", f.worker)
	r.Header.Set("Idempotency-Key", "shutdown-seed")
	r.Header.Set("Prefer", "wait=0")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var receipt historycollect.Receipt
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal("canonical queued seed failed", w.Code)
	}
	snapshot := func() string {
		var values []string
		for _, table := range []string{"history_collections", "history_collection_pages", "history_collection_seen", "receipts", "events"} {
			rows, err := store.DB().Query("SELECT * FROM " + table + " ORDER BY 1,2")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				v := make([]any, len(columns))
				dest := make([]any, len(v))
				for i := range v {
					dest[i] = &v[i]
				}
				if err = rows.Scan(dest...); err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, table+string(b))
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
		return strings.Join(values, "\n")
	}
	before := snapshot()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	// The actual daemon starts with collection disabled, preserving this queued
	// work without launching a provider lane. Its real HTTP request remains open.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout, stderr := &output{ready: make(chan struct{})}, &output{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0"}, stdout, stderr) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
				joined = true
			case <-time.After(8 * time.Second):
				t.Error("owned daemon did not join cleanup")
			}
		}
	})
	select {
	case <-stdout.ready:
	case code := <-done:
		joined = true
		t.Fatal("daemon startup failed", code)
	case <-time.After(8 * time.Second):
		t.Fatal("daemon startup deadline")
	}
	var listening map[string]string
	if json.Unmarshal([]byte(stdout.String()), &listening) != nil || listening["instance_id"] != f.instance {
		t.Fatal("daemon identity changed")
	}
	clientCtx, cancelClient := context.WithCancel(t.Context())
	defer cancelClient()
	request, err := http.NewRequestWithContext(clientCtx, "GET", "http://"+listening["address"]+"/api/v1/projects/"+f.project+"/history/collections/"+receipt.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = r.Header.Clone()
	request.Header.Set("Prefer", "wait=900")
	sent := make(chan struct{})
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			close(sent)
		}
	}}))
	type result struct {
		status int
		err    error
	}
	responseDone := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
		if err != nil {
			responseDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		responseDone <- result{resp.StatusCode, err}
	}()
	clientJoined := false
	t.Cleanup(func() {
		cancelClient()
		if !clientJoined {
			select {
			case <-responseDone:
				clientJoined = true
			case <-time.After(8 * time.Second):
				t.Error("owned collection client did not join cleanup")
			}
		}
	})
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("GET was not sent")
	}
	select {
	case got := <-responseDone:
		clientJoined = true
		t.Fatal("pending GET returned before shutdown", got.status, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	started := time.Now()
	cancel()
	var code int
	select {
	case code = <-done:
		joined = true
	case <-time.After(8 * time.Second):
		t.Fatal("daemon did not join")
	}
	var got result
	select {
	case got = <-responseDone:
		clientJoined = true
	case <-time.After(time.Second):
		t.Fatal("GET client did not join")
	}
	store, err = core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if snapshot() != before {
		t.Fatal("shutdown mutated collection, captures, receipts or events")
	}
	t.Logf("actual daemon shutdown: exit=%d elapsed=%s HTTP=%d transport_error=%t retained_rows_unchanged=true", code, time.Since(started), got.status, got.err != nil)
	if code != 0 || stderr.String() != "" {
		t.Fatal("pending collection GET forced daemon shutdown", code)
	}
	if time.Since(started) > 2*time.Second || got.err != nil || got.status != 503 {
		t.Fatal("observer did not detach promptly with nonterminal error", got.status, got.err)
	}
}
