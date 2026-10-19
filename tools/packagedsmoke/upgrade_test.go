//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

// This reconstructs the supported add-only jobs4 schema, with other components
// current. It proves packaged migration composition, not old-binary compatibility,
// container mounts, or disaster Restore (which deliberately replaces identity).
func TestPackagedUpgradeSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private upgrade fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must be absolute")
	}
	name := "fcd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(binDir, name)
	hash := binaryHash(t, binary)
	t.Logf("upgrade packaged input fcd sha256=%x", hash)
	t.Cleanup(func() {
		if binaryHash(t, binary) != hash {
			t.Error("upgrade smoke modified fcd")
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	source := filepath.Join(private, "source")
	bootstrap := startDaemon(t, ctx, binary, source, env)
	health(t, ctx, bootstrap.address)
	admin, err := os.ReadFile(filepath.Join(source, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("upgrade bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, recipient, credential := seedAccess(t, ctx, source, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	token, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal("upgrade fixture credential unavailable")
	}
	store, err := core.Open(ctx, source)
	if err != nil {
		t.Fatal("stopped upgrade fixture unavailable")
	}
	t.Cleanup(func() { _ = store.Close() })
	mux := server.New(store)
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	prefix := "/api/v1/projects/" + project
	request := func(method, path, actor, key string, body []byte) *http.Request {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		req.Header.Set("X-FC-Instance", bootstrap.instance)
		req.Header.Set("X-FC-Worker", actor)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		return req
	}
	local := func(method, path, actor, key string, body []byte, status int) []byte {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request(method, path, actor, key, body))
		if response.Code != status {
			t.Fatalf("trusted upgrade handler failed (status=%d)", response.Code)
		}
		return bytes.Clone(response.Body.Bytes())
	}
	create := []byte(`{"title":"Retained upgrade job","brief":"Original <literal> brief","status":"active","worker_id":"` + worker + `"}`)
	receipt := local("POST", prefix+"/jobs", worker, "upgrade-create", create, 201)
	var created struct {
		ID string `json:"id"`
	}
	decode(t, receipt, &created)
	if created.ID == "" {
		t.Fatal("upgrade job missing")
	}
	job := prefix + "/jobs/" + created.ID
	local("PATCH", job, worker, "upgrade-edit", []byte(`{"expected_revision":1,"brief":"Current <literal> brief"}`), 200)
	local("POST", job+"/checkpoints", worker, "upgrade-checkpoint", []byte(`{"expected_job_revision":2,"done":"Retained checkpoint","next":"Continue explicitly","blocker":"","evidence":["synthetic-reference"]}`), 201)
	local("POST", prefix+"/messages", worker, "upgrade-message", []byte(`{"recipient_worker_id":"`+recipient+`","body":"Retained <literal> message"}`), 201)
	reads := []struct{ path, actor string }{{job, worker}, {job + "/history?limit=20", worker}, {job + "/checkpoints?limit=20", worker}, {prefix + "/inbox/" + recipient + "?limit=20", recipient}}
	expected := make([][]byte, len(reads))
	for i, r := range reads {
		expected[i] = local("GET", r.path, r.actor, "", nil, 200)
	}
	// No business rows/receipts are fabricated. Only later add-only empty tables
	// are removed, matching the existing public jobs4 migration fixture pattern.
	for _, table := range []string{"note_revisions", "notes", "worker_pause_history", "worker_pause_state"} {
		var rows int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("jobs4 fixture reconstruction requires empty %s", table)
		}
	}
	if _, err = store.DB().ExecContext(ctx, `DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history; DROP TABLE worker_pause_state; UPDATE metadata SET value='4' WHERE key='schema:jobs'`); err != nil {
		t.Fatal("jobs4 fixture reconstruction failed")
	}
	if err = store.Close(); err != nil {
		t.Fatal("stopped fixture did not close")
	}
	before := readUpgradeState(t, ctx, source)
	if before.version != 2 || before.metadata["schema:jobs"] != "4" || before.counts["jobs"] != 1 || before.counts["job_history"] != 2 || before.counts["job_checkpoints"] != 1 || before.counts["inbox_messages"] != 1 || before.counts["receipts"] < 8 {
		t.Fatal("upgrade fixture lacks truthful retained state")
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_source_refreshes", "review_checkpoint_publications"} {
		if before.counts[table] != 0 {
			t.Fatal("upgrade fixture contains native work")
		}
	}
	sourceHash := binaryHash(t, filepath.Join(source, "firecontroller.sqlite"))
	backupCtx, stopBackup := context.WithTimeout(ctx, stepTimeout)
	bundle, err := backup.CreateFromDirectory(backupCtx, source, filepath.Join(private, "bundles"))
	stopBackup()
	if err != nil || bundle.ArtifactCount != 0 {
		t.Fatal("canonical jobs4 backup unavailable")
	}
	bundleHashes := upgradeFileHashes(t, bundle.Path)
	inspectCtx, stopInspect := context.WithTimeout(ctx, stepTimeout)
	_, err = backup.Inspect(inspectCtx, bundle.Path)
	stopInspect()
	if err != nil {
		t.Fatal("canonical jobs4 inspection failed")
	}
	if binaryHash(t, filepath.Join(source, "firecontroller.sqlite")) != sourceHash || !reflect.DeepEqual(before, readUpgradeState(t, ctx, source)) || !reflect.DeepEqual(bundleHashes, upgradeFileHashes(t, bundle.Path)) {
		t.Fatal("backup or inspection mutated retained source/bundle")
	}
	upgraded := filepath.Join(private, "upgrade")
	if os.Mkdir(upgraded, 0700) != nil {
		t.Fatal("exclusive upgrade directory unavailable")
	}
	copied, err := os.ReadFile(filepath.Join(bundle.Path, "firecontroller.sqlite"))
	if err != nil || os.WriteFile(filepath.Join(upgraded, "firecontroller.sqlite"), copied, 0600) != nil {
		t.Fatal("canonical upgrade copy unavailable")
	}
	if !reflect.DeepEqual(before, readUpgradeState(t, ctx, upgraded)) {
		t.Fatal("canonical copy lost retained state")
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: stepTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	httpRequests := 0
	remote := func(daemon *daemonProcess, method, path, actor, key string, body []byte, status int) []byte {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		req := request(method, path, actor, key, body).WithContext(bounded)
		req.URL.Scheme, req.URL.Host = "http", daemon.address
		req.RequestURI = ""
		response, err := client.Do(req)
		httpRequests++
		if err != nil {
			t.Fatal("packaged upgrade HTTP failed")
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		if err != nil || response.StatusCode != status || len(raw) > 64<<10 || !json.Valid(raw) || bytes.Contains(raw, bytes.TrimSpace(token)) || bytes.Contains(raw, bytes.TrimSpace(admin)) {
			t.Fatal("packaged upgrade HTTP returned invalid/private output")
		}
		return raw
	}
	var migrated upgradeState
	for round := 0; round < 2; round++ {
		daemon := startDaemon(t, ctx, binary, upgraded, env)
		if daemon.instance != bootstrap.instance {
			t.Fatal("planned upgrade changed identity")
		}
		health(t, ctx, daemon.address)
		state := readUpgradeState(t, ctx, upgraded)
		if round == 0 {
			assertJobsFourUpgrade(t, before, state)
			migrated = state
		} else if !reflect.DeepEqual(migrated, state) {
			t.Fatal("restart changed retained state")
		}
		for i, r := range reads {
			if !bytes.Equal(expected[i], remote(daemon, "GET", r.path, r.actor, "", nil, 200)) {
				t.Fatal("upgrade changed retained public read")
			}
		}
		if !bytes.Equal(receipt, remote(daemon, "POST", prefix+"/jobs", worker, "upgrade-create", create, 201)) {
			t.Fatal("upgrade lost exact original receipt")
		}
		if !reflect.DeepEqual(migrated, readUpgradeState(t, ctx, upgraded)) {
			t.Fatal("reads/replay added history/events or changed retained rows")
		}
		daemon.stop(t)
		if !reflect.DeepEqual(migrated, readUpgradeState(t, ctx, upgraded)) {
			t.Fatal("daemon stop changed retained state")
		}
	}
	if external.Load() != 0 || !reflect.DeepEqual(bundleHashes, upgradeFileHashes(t, bundle.Path)) || binaryHash(t, filepath.Join(source, "firecontroller.sqlite")) != sourceHash {
		t.Fatal("upgrade changed protected inputs or attempted external HTTP")
	}
	t.Logf("reconstructed jobs4 upgrade passed: total=%s retained_tables_including_sqlite_sequence=%d new_empty_tables=4 http=%d source_bundle_unchanged=true external_http=0 owned_processes=0; other components current, no predecessor-binary/container claim", time.Since(started).Round(time.Millisecond), len(before.tables), httpRequests)
}

type upgradeState struct {
	version  int
	metadata map[string]string
	tables   map[string][32]byte
	counts   map[string]int
}

// Snapshot only; never call core.Open here, which could migrate before fcd.
func readUpgradeState(t *testing.T, ctx context.Context, data string) upgradeState {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	path := filepath.ToSlash(filepath.Join(data, "firecontroller.sqlite"))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("upgrade snapshot unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("upgrade snapshot transaction unavailable")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatal("upgrade snapshot is not read-only")
	}
	var integrity string
	if err = tx.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("upgrade integrity failure")
	}
	fk, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal("upgrade foreign-key check unavailable")
	}
	invalid := fk.Next()
	fkErr := fk.Err()
	fk.Close()
	if invalid || fkErr != nil {
		t.Fatal("upgrade foreign-key failure")
	}
	out := upgradeState{metadata: map[string]string{}, tables: map[string][32]byte{}, counts: map[string]int{}}
	if tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&out.version) != nil {
		t.Fatal("upgrade base version unavailable")
	}
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM metadata ORDER BY key`)
	if err != nil {
		t.Fatal("upgrade metadata unavailable")
	}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			t.Fatal("upgrade metadata unreadable")
		}
		out.metadata[k] = v
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		t.Fatal("upgrade metadata interrupted")
	}
	rows, err = tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' AND (name NOT LIKE 'sqlite_%' OR name='sqlite_sequence') ORDER BY name`)
	if err != nil {
		t.Fatal("upgrade table inventory unavailable")
	}
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			t.Fatal("upgrade inventory unreadable")
		}
		names = append(names, name)
	}
	rowErr = rows.Err()
	rows.Close()
	if rowErr != nil {
		t.Fatal("upgrade inventory interrupted")
	}
	for _, name := range names {
		rows, err = tx.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(name, `"`, `""`)+`" ORDER BY rowid`)
		if err != nil {
			t.Fatal("upgrade retained table unavailable")
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal("upgrade retained columns unavailable")
		}
		h := sha256.New()
		encoder := json.NewEncoder(h)
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if rows.Scan(ptrs...) != nil || encoder.Encode(values) != nil {
				t.Fatal("upgrade retained rows unreadable")
			}
			out.counts[name]++
		}
		rowErr = rows.Err()
		rows.Close()
		if rowErr != nil {
			t.Fatal("upgrade retained read interrupted")
		}
		if _, ok := out.counts[name]; !ok {
			out.counts[name] = 0
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		out.tables[name] = sum
	}
	return out
}

func assertJobsFourUpgrade(t *testing.T, before, after upgradeState) {
	t.Helper()
	expected := maps.Clone(before.metadata)
	expected["schema:jobs"] = "6"
	if before.version != 2 || after.version != 2 || !reflect.DeepEqual(expected, after.metadata) {
		t.Fatal("upgrade changed undeclared metadata/base")
	}
	added := []string{"worker_pause_state", "worker_pause_history", "notes", "note_revisions"}
	if len(after.tables) != len(before.tables)+len(added) {
		t.Fatal("upgrade table inventory differs")
	}
	for _, name := range added {
		if _, old := before.tables[name]; old {
			t.Fatal("jobs4 fixture contains later tables")
		}
		if _, ok := after.tables[name]; !ok || after.counts[name] != 0 {
			t.Fatal("upgrade invented coordination records")
		}
	}
	for name, hash := range before.tables {
		if after.counts[name] != before.counts[name] || name != "metadata" && after.tables[name] != hash {
			t.Fatal("upgrade changed retained table", name)
		}
	}
}

func upgradeFileHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			t.Fatal("upgrade bundle contains symlink")
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = binaryHash(t, path)
		return nil
	})
	if err != nil {
		t.Fatal("upgrade bundle inventory unavailable")
	}
	return out
}
