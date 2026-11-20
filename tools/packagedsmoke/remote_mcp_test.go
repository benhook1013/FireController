//go:build packaged_smoke

package packagedsmoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const packagedConnectorHost = "controller.example.com"

type remoteSmokeTransport struct {
	base  *http.Transport
	token string
}

func (r remoteSmokeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = packagedConnectorHost
	req.Header.Set("Authorization", "Bearer "+r.token)
	return r.base.RoundTrip(req)
}

func TestPackagedRemoteMCP(t *testing.T) {
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("cannot secure private fixture directory")
	}
	ctx, stop := context.WithTimeout(context.Background(), 50*time.Second)
	t.Cleanup(stop)
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir = "../../bin"
	}
	binDir, _ = filepath.Abs(binDir)
	fc, fcd := filepath.Join(binDir, "fc"), filepath.Join(binDir, "fcd")
	if runtime.GOOS == "windows" {
		fc, fcd = fc+".exe", fcd+".exe"
	}
	hash := binaryHash(t, fcd)
	t.Logf("remote MCP packaged fcd sha256=%x", hash)
	t.Cleanup(func() {
		if binaryHash(t, fcd) != hash {
			t.Error("packaged candidate changed")
		}
	})
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	predecessor := os.Getenv("FC_REMOTE_MCP_PREDECESSOR")
	first := fcd
	if predecessor != "" {
		first = predecessor
	}
	initial := startDaemon(t, ctx, first, data, env)
	requireRemoteSmokeStatus(t, ctx, "http://"+initial.address+"/mcp", "", "", http.StatusNotFound)
	initial.stop(t)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("synthetic bootstrap credential unavailable")
	}
	online := startDaemon(t, ctx, fcd, data, env)
	online.stop(t)
	if online.instance != initial.instance {
		t.Fatal("startup migration changed instance identity")
	}
	project, worker, other, _ := seedAccess(t, ctx, data, online.instance, strings.TrimSpace(string(admin)), private)
	oldVersionFence := func() {
		if predecessor == "" {
			return
		}
		bounded, cancel := context.WithTimeout(ctx, stepTimeout)
		defer cancel()
		cmd := exec.CommandContext(bounded, predecessor, "--data-dir", data, "--listen", "127.0.0.1:0")
		cmd.Env = env
		var stdout, stderr boundedOutput
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var exit *exec.ExitError
		err := cmd.Run()
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || stdout.len() != 0 || !strings.Contains(string(stderr.bytes()), `"code":"schema_unsupported"`) {
			t.Fatal("authentic predecessor did not refuse the v4 database before listening")
		}
		t.Log("authentic predecessor v3 migrated and predecessor startup fenced on v4")
	}
	oldVersionFence()

	bundle := func() backup.Summary {
		t.Helper()
		var made, checked backup.Summary
		decode(t, runRemoteSmokeOperator(t, ctx, fcd, env, "backup", "--data-dir", data, "--destination", filepath.Join(private, "bundles")), &made)
		decode(t, runRemoteSmokeOperator(t, ctx, fcd, env, "backup-inspect", "--bundle", made.Path), &checked)
		if made.State != "complete" || checked.State != "verified" || made.ID == "" || made.ID != checked.ID || made.SourceInstanceID != checked.SourceInstanceID {
			t.Fatal("packaged backup inspection disagrees with creation")
		}
		return made
	}
	empty := bundle()
	var unused backup.RestoreSummary
	decode(t, runRemoteSmokeOperator(t, ctx, fcd, env, "restore", "--bundle", empty.Path, "--data-dir", filepath.Join(private, "empty-restore")), &unused)
	if unused.State != "inactive" || unused.SourceInstanceID != online.instance {
		t.Fatal("empty-connector v4 restore was not inactive")
	}

	bearer := core.NewID("synthetic")
	bearerFile := writeSmokeToken(t, private, "connector.token", bearer)
	online = startDaemon(t, ctx, fcd, data, env, "--remote-mcp-origin", "https://"+packagedConnectorHost)
	direct := "http://" + online.address
	prefix := "/api/v1/projects/" + project
	issue := func(base, instance, adminPath, secretPath, key string) {
		t.Helper()
		profilePath := filepath.Join(private, key+".json")
		runCLI(t, ctx, fc, env, "--profiles-file", profilePath, "--profile", "operator", "profile", "create", "--server", base, "--instance", instance, "--project", project, "--worker", worker, "--credential-file", adminPath)
		var grant client.ProjectCredentialReceipt
		decode(t, runCLI(t, ctx, fc, env, "--profiles-file", profilePath, "--profile", "operator", "credentials", "grant", "--connector-worker", worker, "--credential-file", secretPath, "--permission", "read", "--permission", "write", "--key", key), &grant)
		if grant.ConnectorWorkerID != worker || grant.ProjectID != project || grant.ID == "" {
			t.Fatal("packaged connector provisioning receipt lost its binding")
		}
	}
	issue(direct, online.instance, adminFile, bearerFile, "grant-original")
	gateway := direct
	if nginx := os.Getenv("FC_REMOTE_MCP_NGINX"); nginx != "" {
		gateway = startRemoteSmokeNginx(t, nginx, online.address, env)
	}
	requireRemoteSmokeStatus(t, ctx, gateway+"/mcp", "foreign.example.com", bearer, http.StatusForbidden)
	requireRemoteSmokeStatus(t, ctx, gateway+"/mcp", packagedConnectorHost, "", http.StatusUnauthorized)
	requireRemoteSmokeStatus(t, ctx, gateway+"/mcp", packagedConnectorHost, bearer, http.StatusMethodNotAllowed)

	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "packaged-remote-smoke", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: gateway + "/mcp", DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: remoteSmokeTransport{base: transport, token: bearer}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var catalogCount int
	var cursor string
	for page := 1; page <= 8; page++ {
		got, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		catalogCount += len(got.Tools)
		cursor = got.NextCursor
		if cursor == "" {
			if page < 2 {
				t.Fatal("packaged discovery did not paginate")
			}
			break
		}
	}
	if cursor != "" {
		t.Fatal("remote discovery exceeded its page budget")
	}
	var snapshot client.InboxSnapshot
	decodeRemoteSmokeTool(t, ctx, cs, "inbox_snapshot", nil, &snapshot)
	var event client.MessageEventPage
	decodeRemoteSmokeTool(t, ctx, cs, "wait_for_messages", map[string]any{"cursor": snapshot.EventCursor, "wait_seconds": 1}, &event)
	if !event.TimedOut {
		t.Fatal("packaged wait did not return a bounded timeout")
	}
	write := map[string]any{"title": "Packaged connector restore proof", "worker_id": worker, "idempotency_key": "remote-packaged-create"}
	var original, replay client.JobReceipt
	decodeRemoteSmokeTool(t, ctx, cs, "jobs_create", write, &original)
	decodeRemoteSmokeTool(t, ctx, cs, "jobs_create", write, &replay)
	if original.ID == "" || original != replay {
		t.Fatal("packaged guarded write replay lost its receipt")
	}
	var job client.Job
	decodeRemoteSmokeTool(t, ctx, cs, "job_read", map[string]any{"job_id": original.ID}, &job)
	if job.ID != original.ID || job.WorkerID != worker {
		t.Fatal("packaged read lost committed worker attribution")
	}
	t.Logf("packaged discovery %d tools across pages, bounded wait, write/read and exact replay", catalogCount)
	online.stop(t)

	bound := bundle()
	restoredDir := filepath.Join(private, "bound-restore")
	var restored backup.RestoreSummary
	decode(t, runRemoteSmokeOperator(t, ctx, fcd, env, "restore", "--bundle", bound.Path, "--data-dir", restoredDir), &restored)
	if restored.State != "inactive" || restored.InstanceID == online.instance || restored.SourceInstanceID != online.instance {
		t.Fatal("bound v4 restore did not acquire an inactive replacement identity")
	}
	freshAdmin := writeSmokeToken(t, private, "fresh-admin.token", core.NewID("synthetic"))
	runRemoteSmokeOperator(t, ctx, fcd, env, "restore-activate", "--data-dir", restoredDir, "--expect-instance", restored.InstanceID, "--fenced-predecessor", online.instance, "--admin-token-file", freshAdmin)
	replacement := startDaemon(t, ctx, fcd, restoredDir, env, "--remote-mcp-origin", "https://"+packagedConnectorHost)
	replacementURL := "http://" + replacement.address
	requireRemoteSmokeStatus(t, ctx, replacementURL+"/mcp", packagedConnectorHost, bearer, 401)
	requireRemoteSmokeStatus(t, ctx, replacementURL+"/api/v1/instance", "", bearer, 401)
	fresh := core.NewID("synthetic")
	issue(replacementURL, replacement.instance, freshAdmin, writeSmokeToken(t, private, "fresh-connector.token", fresh), "grant-replacement")
	requireRemoteSmokeStatus(t, ctx, replacementURL+"/mcp", packagedConnectorHost, fresh, 405)
	requireRemoteSmokeStatus(t, ctx, replacementURL+prefix, "", fresh, 403)
	readReplacement := func(acting string, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", replacementURL+prefix+"/jobs/"+original.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+fresh)
		req.Header.Set("X-FC-Worker", acting)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("restored read status=%d, want %d", resp.StatusCode, want)
		}
	}
	readReplacement(worker, 200)
	readReplacement(other, 403)
	t.Log("packaged v4 backup, inspect and restore passed with and without bindings; activated copy refused the old connector and scoped a freshly issued connector")
}

func runRemoteSmokeOperator(t *testing.T, ctx context.Context, binary string, env []string, args ...string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stderr.len() > 64<<10 || !json.Valid(stdout.bytes()) {
		var diagnostic struct {
			Error struct{ Code string }
		}
		_ = json.Unmarshal(stderr.bytes(), &diagnostic)
		t.Fatalf("packaged %s failed (diagnostic=%q, stderr bytes=%d)", args[0], diagnostic.Error.Code, stderr.len())
	}
	return stdout.bytes()
}

func requireRemoteSmokeStatus(t *testing.T, ctx context.Context, endpoint, host, secret string, want int) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("HTTP status=%d want=%d", resp.StatusCode, want)
	}
}

func writeSmokeToken(t *testing.T, dir, name, token string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal("cannot write synthetic private credential")
	}
	return path
}

func decodeRemoteSmokeTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any, dst any) {
	t.Helper()
	out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || out.IsError {
		t.Fatalf("packaged remote tool %s failed: %v", name, err)
	}
	data, err := json.Marshal(out.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	decode(t, data, dst)
}

func startRemoteSmokeNginx(t *testing.T, nginx, daemon string, env []string) string {
	t.Helper()
	prefix := t.TempDir()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddress := listener.Addr().String()
	listener.Close()
	if err := os.MkdirAll(filepath.Join(prefix, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	include, err := os.ReadFile("../../examples/remote-mcp.nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	includePath := filepath.Join(prefix, "remote-mcp.conf")
	if err := os.WriteFile(includePath, []byte(strings.ReplaceAll(string(include), "127.0.0.1:8081", daemon)), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("daemon off; worker_processes 1; pid nginx.pid; error_log stderr crit; events {} http { server { listen %s; server_name _; include remote-mcp.conf; } }", proxyAddress)
	if err := os.WriteFile(filepath.Join(prefix, "nginx.conf"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := exec.Command(nginx, "-p", prefix+string(os.PathSeparator), "-c", "nginx.conf")
	proxy.Env = env
	var stderr boundedOutput
	proxy.Stderr = &stderr
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Wait() }()
	t.Cleanup(func() {
		_ = proxy.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(stepTimeout):
			_ = proxy.Process.Kill()
			<-done
			t.Error("disposable Nginx did not stop on interrupt")
		}
	})
	deadline := time.Now().Add(stepTimeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", proxyAddress, 50*time.Millisecond)
		if err == nil {
			connection.Close()
			t.Log("packaged HTTP qualification uses disposable Nginx with the committed route include")
			return "http://" + proxyAddress
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("disposable Nginx listener unavailable")
	return ""
}
