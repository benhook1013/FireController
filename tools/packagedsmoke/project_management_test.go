//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPackagedProjectManagementSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if bin == "" {
		bin, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute binary directory required")
	}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		t.Logf("administration input %s sha256=%x", name, binaryHash(t, filepath.Join(bin, name)))
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "source")
	parent := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	project, worker := "", ""
	if parent != "" {
		source := filepath.Join(parent, "source")
		err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			dest := filepath.Join(data, rel)
			if d.IsDir() {
				return os.MkdirAll(dest, 0700)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(dest, b, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]string
		b, err := os.ReadFile(filepath.Join(parent, "fixture.json"))
		if err != nil || json.Unmarshal(b, &f) != nil {
			t.Fatal("parent metadata", err)
		}
		project, worker = f["project"], f["worker"]
	} else {
		bootstrap := startDaemon(t, ctx, filepath.Join(bin, "fcd"), data, env)
		admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
		if err != nil {
			t.Fatal(err)
		}
		bootstrap.stop(t)
		project, worker, _, _ = seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	}
	daemon := startDaemon(t, ctx, filepath.Join(bin, "fcd"), data, env)
	health(t, ctx, daemon.address)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	managerFile := filepath.Join(private, "manager.token")
	readerFile := filepath.Join(private, "reader.token")
	const manager = "synthetic-packaged-management-token"
	const reader = "synthetic-packaged-reader-token-v1"
	for file, token := range map[string]string{managerFile: manager, readerFile: reader} {
		if err := os.WriteFile(file, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := func(name, token string) string {
		t.Helper()
		path := filepath.Join(private, name+".json")
		runCLI(t, ctx, filepath.Join(bin, "fc"), env, "--profiles-file", path, "--profile", name, "profile", "create", "--server", "http://"+daemon.address, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", token)
		return path
	}
	adminProfile := profile("admin", adminFile)
	cli := func(file, name string, args ...string) json.RawMessage {
		t.Helper()
		raw := runCLI(t, ctx, filepath.Join(bin, "fc"), env, append([]string{"--profiles-file", file, "--profile", name}, args...)...)
		if bytes.Contains(raw, []byte(manager)) || bytes.Contains(raw, []byte(reader)) {
			t.Fatal("credential in output")
		}
		return raw
	}
	var owner client.ProjectCredentialReceipt
	decode(t, cli(adminProfile, "admin", "credentials", "grant", "--credential-file", managerFile, "--permission", "manage", "--key", "grant-manager"), &owner)
	managerProfile := profile("manager", managerFile)
	// Scheduling configuration is independent of execution configuration and keeps
	// exact control identities across native CLI/MCP and restart.
	var scheduling client.ReviewScheduling
	decode(t, cli(managerProfile, "manager", "reviews", "scheduling"), &scheduling)
	if scheduling.Revision != 0 || scheduling.AutomaticHosted || scheduling.AutomaticCLI {
		t.Fatal("new scheduling is not off", scheduling)
	}
	configured := cli(managerProfile, "manager", "reviews", "scheduling", "configure", "--automatic-hosted", "false", "--automatic-cli", "false", "--expect-revision", "0", "--key", "scheduling-off")
	if !bytes.Equal(configured, cli(managerProfile, "manager", "reviews", "scheduling", "configure", "--automatic-hosted", "false", "--automatic-cli", "false", "--expect-revision", "0", "--key", "scheduling-off")) {
		t.Fatal("scheduling exact replay changed")
	}

	var grant client.ProjectCredentialReceipt
	decode(t, cli(managerProfile, "manager", "credentials", "grant", "--credential-file", readerFile, "--permission", "read", "--key", "grant-reader"), &grant)
	permissionFile := filepath.Join(private, "permissions.json")
	if err := os.WriteFile(permissionFile, []byte(`{"expected_permissions":["read"],"permissions":["read","write"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	first := cli(managerProfile, "manager", "credentials", "update", grant.ID, "--file", permissionFile, "--key", "update-reader")
	if !bytes.Equal(first, cli(managerProfile, "manager", "credentials", "update", grant.ID, "--file", permissionFile, "--key", "update-reader")) {
		t.Fatal("CAS receipt replay changed")
	}
	var target client.WorkerCreateReceipt
	decode(t, cli(managerProfile, "manager", "workers", "create", "--name", "Retiring worker", "--key", "create-target"), &target)
	var message client.MessageReceipt
	decode(t, cli(managerProfile, "manager", "inbox", "send", "--to", target.ID, "--body", "Synthetic cleanup message", "--key", "pending-message"), &message)
	var preview client.WorkerRetirement
	decode(t, cli(managerProfile, "manager", "workers", "retirement", target.ID), &preview)
	if preview.CanRetire || preview.Blockers.Messages.Count != 1 {
		t.Fatal("pending message did not block", preview)
	}
	command := exec.CommandContext(ctx, filepath.Join(bin, "fcmcp"), "--profiles-file", managerProfile, "--profile", "manager")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "administration-smoke", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("MCP connect", err)
	}
	call := func(name string, args any) json.RawMessage {
		t.Helper()
		value, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || value.IsError {
			t.Fatal("MCP operation", name, err, value)
		}
		for _, content := range value.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				return json.RawMessage(text.Text)
			}
		}
		t.Fatal("missing MCP text")
		return nil
	}

	call("review_pull_scheduling_update", map[string]any{"pull_number": 42, "paused": true, "reason": "Synthetic scheduling hold", "expected_revision": 0, "idempotency_key": "scheduling-pause"})
	var paused client.ReviewPullScheduling
	decode(t, call("review_pull_scheduling_read", map[string]any{"pull_number": 42}), &paused)
	if !paused.Paused || paused.Revision != 1 {
		t.Fatal("MCP pause not retained", paused)
	}
	call("review_pull_scheduling_update", map[string]any{"pull_number": 42, "paused": false, "expected_revision": 1, "idempotency_key": "scheduling-resume"})
	call("inbox_ack", map[string]any{"message_id": message.ID, "worker_id": target.ID, "idempotency_key": "peer-ack"})
	var retired client.WorkerRetirementReceipt
	decode(t, call("worker_retire", map[string]any{"worker_id": target.ID, "idempotency_key": "retire"}), &retired)
	if retired.Outcome != "retired" {
		t.Fatal(retired)
	}
	if err := session.Close(); err != nil || stderr.len() != 0 {
		t.Fatal("MCP shutdown", err)
	}
	var workers client.Page[client.Worker]
	decode(t, cli(managerProfile, "manager", "workers", "list", "--include-retired"), &workers)
	found := false
	for _, w := range workers.Items {
		if w.ID == target.ID {
			found = w.Retired
		}
	}
	if !found {
		t.Fatal("retired history absent")
	}
	daemon.stop(t)
	daemon = startDaemon(t, ctx, filepath.Join(bin, "fcd"), data, env)
	managerProfile = profile("manager-restarted", managerFile)
	var resumed client.ReviewPullScheduling
	decode(t, cli(managerProfile, "manager-restarted", "reviews", "scheduling", "read", "--pr", "42"), &resumed)
	if resumed.Paused || resumed.Revision != 2 || resumed.Reason != "" {
		t.Fatal("restart changed scheduling control", resumed)
	}
	decode(t, cli(managerProfile, "manager-restarted", "reviews", "scheduling"), &scheduling)
	if scheduling.Revision != 1 || scheduling.AutomaticHosted || scheduling.AutomaticCLI {
		t.Fatal("restart changed default-off control", scheduling)
	}

	var replay client.WorkerRetirementReceipt
	decode(t, cli(managerProfile, "manager-restarted", "workers", "retire", target.ID, "--key", "retire"), &replay)
	if replay != retired {
		t.Fatal("restart changed retirement receipt")
	}
	// Reader/writer cannot administer even though its ordinary write grant is live.
	payload := strings.NewReader(`{"expected_permissions":["read","write"],"permissions":["manage"]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+daemon.address+"/api/v1/projects/"+project+"/credentials/"+grant.ID, payload)
	req.Header.Set("Authorization", "Bearer "+reader)
	req.Header.Set("Idempotency-Key", "writer-cannot-promote")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("writer gained management", resp.StatusCode)
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("administration caused provider/network work", external.Load())
	}
}
