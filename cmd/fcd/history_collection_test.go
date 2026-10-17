package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
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
