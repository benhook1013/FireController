//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Actual daemon HTTP configuration is sufficient for visible enrollment/order,
// even while both independent GitHub observation lanes are deliberately blocked.
func TestPackagedExecutionQueueEnrollmentSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX executable fixture and child CA trust qualified on Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if bin == "" {
		bin, _ = filepath.Abs("../../bin")
	}
	daemonBinary := filepath.Join(bin, "fcd")
	beforeHash := binaryHash(t, daemonBinary)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret)
	tokenFile := filepath.Join(private, "provider.token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	provider := newRunSelectionHTTPSFixture(t, private, token)
	stub := filepath.Join(private, "never-run")
	stubLog := filepath.Join(private, "unexpected-command")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf 'unexpected\\n' >> "+runSelectionShellQuote(stubLog)+"\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(private, "catalog.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "capabilities": []any{map[string]any{"id": "queue-smoke", "account": "synthetic-account", "organization": "synthetic-org", "channels": []string{"cli"}, "github_credential_file": tokenFile, "git_executable": stub, "cli_executable": stub, "authentication_environment": map[string]string{}}}})
	if err := os.WriteFile(catalog, raw, 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + provider.proxy, "HTTPS_PROXY=" + provider.proxy, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + provider.ca, "SSL_CERT_DIR=" + private}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, daemonBinary, data, env)
	adminBytes, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap credential unavailable")
	}
	admin := strings.TrimSpace(string(adminBytes))
	bootstrap.stop(t)
	project, worker, _, workerFile := seedAccess(t, ctx, data, bootstrap.instance, admin, private)
	gate := provider.holdNextObserver(t)
	daemon := startDaemon(t, ctx, daemonBinary, data, env, "--review-catalog", catalog)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	prefix := "http://" + daemon.address + "/api/v1/projects/" + project + "/reviews"
	call := func(method, path, key string, body []byte) []byte {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, prefix+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", bootstrap.instance)
		r.Header.Set("X-FC-Worker", worker)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
			r.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("packaged queue request failed", err)
		}
		defer response.Body.Close()
		out, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("queue request status %d", response.StatusCode)
		}
		if bytes.Contains(out, adminBytes) || bytes.Contains(out, []byte(token)) {
			t.Fatal("credential disclosed")
		}
		return out
	}
	configure := func(revision int64, numbers []int64) ([]byte, []byte) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"expected_revision": revision, "capability_id": "queue-smoke", "mode": "independent", "queue": numbers, "channels": []string{"cli"}, "paused": true})
		key := "queue-config-" + string(rune('a'+revision))
		return body, call("POST", "/execution/configuration", key, body)
	}
	readQueue := func(numbers []int64) reviews.QueueResponse {
		t.Helper()
		var q reviews.QueueResponse
		decode(t, call("GET", "/queue?limit=100", "", nil), &q)
		got := []int64{}
		for _, p := range q.Items {
			got = append(got, p.Number)
			if p.HeadSHA != "" || p.State != "unknown" || p.RunCount != 0 {
				t.Fatal("blocked observer invented source/review facts")
			}
		}
		if !reflect.DeepEqual(got, numbers) {
			t.Fatalf("visible queue %v want %v", got, numbers)
		}
		return q
	}
	body, receipt := configure(0, []int64{2})
	readQueue([]int64{2})
	gate.awaitStarted(t, ctx)
	profiles := filepath.Join(private, "profiles.json")
	setup := exec.CommandContext(ctx, filepath.Join(bin, "fc"), "--profiles-file", profiles, "--profile", "builder", "profile", "create", "--server", "http://"+daemon.address, "--instance", bootstrap.instance, "--project", project, "--worker", worker, "--credential-file", workerFile)
	setup.Env = env
	var setupOut, setupErr bytes.Buffer
	setup.Stdout = &setupOut
	setup.Stderr = &setupErr
	if err := setup.Run(); err != nil || setupErr.Len() != 0 {
		var d struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(setupErr.Bytes(), &d)
		t.Fatalf("profile setup failed: %v code=%s message=%s", err, d.Error.Code, d.Error.Message)
	}

	added := runCLI(t, ctx, filepath.Join(bin, "fc"), env, "--profiles-file", profiles, "--profile", "builder", "reviews", "queue-add", "3", "--after", "2", "--expect-config", "1", "--key", "bound-add-three")
	var addedReceipt struct {
		ID       string
		State    string
		Revision int64
	}
	decode(t, added, &addedReceipt)
	if addedReceipt.ID != project || addedReceipt.Revision != 2 {
		t.Fatal("bound CLI add did not commit")
	}

	readQueue([]int64{2, 3})
	command := exec.CommandContext(ctx, filepath.Join(bin, "fcmcp"), "--profiles-file", profiles, "--profile", "builder")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-queue-changes", Version: "1"}, nil)
	connectCtx, stopConnect := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(connectCtx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stopConnect()
	if err != nil {
		if command.Process != nil {
			command.Process.Kill()
		}
		t.Fatal("actual bound MCP connection failed")
	}
	var closeOnce sync.Once
	closeMCP := func() {
		closeOnce.Do(func() {
			if err := session.Close(); err != nil {
				t.Error(err)
			}
			if command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
				t.Error("MCP process did not join cleanly")
			}
		})
	}
	t.Cleanup(closeMCP)
	moveArgs := map[string]any{"expected_configuration_revision": 2, "pull_number": 3, "position": "first", "idempotency_key": "bound-move-three"}
	for range 2 {
		toolCtx, stop := context.WithTimeout(ctx, stepTimeout)
		result, err := session.CallTool(toolCtx, &mcp.CallToolParams{Name: "review_queue_move", Arguments: moveArgs})
		stop()
		if err != nil || result == nil || result.IsError {
			t.Fatal("bound MCP move/replay failed")
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var moved struct {
			ID       string
			State    string
			Revision int64
		}
		decode(t, raw, &moved)
		if moved.ID != project || moved.Revision != 3 {
			t.Fatal("MCP receipt changed", moved)
		}
	}
	closeMCP()
	last := readQueue([]int64{3, 2})
	if !bytes.Equal(receipt, call("POST", "/execution/configuration", "queue-config-a", body)) || readQueue([]int64{3, 2}).SnapshotID != last.SnapshotID {
		t.Fatal("configuration replay published another snapshot")
	}
	daemon.stop(t)
	gate.awaitCancelled(t, ctx)
	if _, err := os.Stat(stubLog); !os.IsNotExist(err) {
		t.Fatal("configuration launched provider process", err)
	}
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("configuration started review work", table, count, err)
		}
	}
	if binaryHash(t, daemonBinary) != beforeHash {
		t.Fatal("daemon binary changed")
	}
	t.Logf("packaged single-queue enrollment/insertion/reorder/replay passed in %s; no display write, provider dispatch or owned process remains", time.Since(started).Round(time.Millisecond))
}
