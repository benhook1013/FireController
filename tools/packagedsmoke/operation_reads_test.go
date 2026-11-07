//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The trusted fixture seeds only a queued local record. Packaged reads must
// expose it without dispatch, provider access or changes to durable accounting.
func TestPackagedOperationReadsSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private operation fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binary directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binaries, hashes := map[string]string{}, map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		binaries[name] = filepath.Join(binDir, name+suffix)
		hashes[name] = binaryHash(t, binaries[name])
	}
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Error("packaged binary changed", name)
			}
		}
	}()
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, name := range []string{"SYSTEMROOT", "WINDIR"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("fixture bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	// A valid paused configuration gives the retained record its original
	// foreign-key parent without enabling any runtime capability.
	_, err = store.DB().ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,1,?)`, project, `{"expected_revision":0,"capability_id":"fixture-capability","mode":"independent","queue":[42],"channels":["cli"],"paused":true}`)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	id := "review_operation_" + strings.Repeat("a", 32)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at) VALUES(?,?,'fixture-actor',?,1,42,'cli',?,'queued',1,'','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, id, project, worker, strings.Repeat("b", 40))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	state := func() []int64 {
		t.Helper()
		s, err := core.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var result []int64
		for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_execution_reservations", "events"} {
			var n int64
			if err = s.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			result = append(result, n)
		}
		return result
	}
	before := state()
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	profilePath := filepath.Join(private, "operations.json")
	prefix := []string{"--profiles-file", profilePath, "--profile", "operations"}
	cliCall := func(args ...string) json.RawMessage {
		t.Helper()
		return runCLI(t, ctx, binaries["fc"], env, append(append([]string{}, prefix...), args...)...)
	}
	cliCall("profile", "create", "--server", "http://"+daemon.address, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	checkPage := func(raw json.RawMessage) {
		t.Helper()
		var page client.ReviewOperationsPage
		decode(t, raw, &page)
		if len(page.Items) != 1 || page.Items[0].ID != id || page.HasMore {
			t.Fatal("packaged discovery lost queued operation")
		}
	}
	checkCompact := func(raw json.RawMessage) {
		t.Helper()
		var value client.ReviewOperationStatus
		decode(t, raw, &value)
		if value.ID != id || value.Stage != "queued" || value.NextAction != "wait" || bytes.Contains(raw, []byte("fixture-actor")) || bytes.Contains(raw, []byte("capability_fingerprint")) {
			t.Fatal("packaged compact projection invalid")
		}
	}
	checkFull := func(raw json.RawMessage) {
		t.Helper()
		var value client.ReviewOperation
		decode(t, raw, &value)
		if value.ID != id || value.ProjectID != project || value.WorkerID != worker {
			t.Fatal("packaged explicit diagnostics lost original identity")
		}
	}
	checkPage(cliCall("reviews", "operations"))
	checkCompact(cliCall("reviews", "operation", id))
	checkFull(cliCall("reviews", "operation", id, "--diagnostics"))
	cmd := exec.CommandContext(ctx, binaries["fcmcp"], prefix...)
	cmd.Env = env
	stderr := &boundedOutput{}
	cmd.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-operation-reads", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("packaged MCP connection failed")
	}
	defer session.Close()
	tool := func(name string, args any) json.RawMessage {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result == nil || result.IsError {
			t.Fatal("packaged MCP read failed", name)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	checkPage(tool("review_operations_list", map[string]any{}))
	checkCompact(tool("review_operation_read", map[string]any{"operation_id": id}))
	checkFull(tool("review_operation_read", map[string]any{"operation_id": id, "diagnostics": true}))
	if err = session.Close(); err != nil || stderr.len() != 0 {
		t.Fatal("packaged MCP shutdown failed")
	}
	daemon.stop(t)
	if !reflect.DeepEqual(before, state()) || external.Load() != 0 {
		t.Fatal("packaged reads changed durable records or contacted a provider")
	}
	t.Log("three packaged CLI reads and three official-SDK MCP reads preserved operation, attempt, evidence, reservation and event counts; no provider requests")
}
