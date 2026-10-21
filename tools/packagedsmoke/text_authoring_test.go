//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/publicsitemodel"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Small edits travel as small edits, while explicit publication still pins the
// selected audience-facing revision. All executable traffic is synthetic local
// traffic; the test never builds binaries or borrows an installed profile.
func TestPackagedTextAuthoringSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private text authoring fixture unavailable")
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
	fc, fcd, fcmcp := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix), filepath.Join(binDir, "fcmcp"+suffix)
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcd: binaryHash(t, fcd), fcmcp: binaryHash(t, fcmcp)}
	t.Cleanup(func() {
		for file, hash := range hashes {
			if binaryHash(t, file) != hash {
				t.Error("packaged input changed")
			}
		}
	})
	var external, patchCalls, patchBytes atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	token, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/map/entries/") {
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 1025))
			r.Body.Close()
			if readErr != nil || len(body) > 1024 || bytes.Contains(body, []byte("UNCHANGED_DESCRIPTION")) || bytes.Contains(body, []byte("UNCHANGED_WHERE")) {
				w.WriteHeader(413)
				return
			}
			patchCalls.Add(1)
			patchBytes.Add(int64(len(body)))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "admin.json")
	base := []string{"--profiles-file", profiles, "--profile", "admin"}
	call := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, fc, env, append(append([]string{}, base...), args...)...)
		if bytes.Contains(body, bytes.TrimSpace(token)) {
			t.Fatal("credential disclosed")
		}
		return body
	}
	refuse := func(args ...string) {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fc, append(append([]string{}, base...), args...)...)
		command.Env = env
		command.WaitDelay = time.Second
		out, diagnostic := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = out, diagnostic
		if command.Run() == nil || out.len() != 0 || diagnostic.len() > 4096 || !json.Valid(diagnostic.bytes()) || bytes.Contains(diagnostic.bytes(), bytes.TrimSpace(token)) {
			t.Fatal("guarded authoring refusal failed")
		}
	}
	write := func(name string, value any) string {
		t.Helper()
		body, e := json.Marshal(value)
		file := filepath.Join(private, name)
		if e != nil || os.WriteFile(file, body, 0600) != nil {
			t.Fatal("private authoring document unavailable")
		}
		return file
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	entry := client.ProjectMapEditable{Kind: "workstream", Title: "Selected roadmap", Description: strings.Repeat("UNCHANGED_DESCRIPTION ", 240), Where: strings.Repeat("UNCHANGED_WHERE ", 280), Next: "Old next milestone", State: "Building", Phases: []client.ProjectMapPhase{{ID: "foundation", Name: "Foundation", State: "Active"}}, Links: []client.ProjectMapLink{}}
	var created client.ProjectMapReceipt
	decode(t, call("map", "create", "--file", write("entry.json", entry), "--key", "authoring-map-create"), &created)
	var job struct {
		ID string `json:"id"`
	}
	decode(t, call("jobs", "create", "--title", "Selected job", "--brief", "PRIVATE_BRIEF_ORIGINAL", "--worker", worker, "--key", "authoring-job-create"), &job)
	call("jobs", "summary-set", job.ID, "--expect-revision", "1", "--expect-summary-revision", "0", "--title", "APPROVED_SUMMARY_ONE", "--body", "Approved audience prose", "--key", "authoring-summary-one")
	manifest := publicsitemodel.Manifest{Slug: "authoring-smoke", Description: "Original project introduction", ProjectMap: []publicsitemodel.Card{{Key: "editorial", Title: "Unrelated editorial card", Description: "Keep independent editorial content", Links: []publicsitemodel.Link{}}}, JobRefs: []publicsitemodel.JobRef{{Key: "selected_job", JobID: job.ID, SummaryRevision: 1}}, MapRefs: []publicsitemodel.MapRef{{Key: "selected_map", EntryID: created.ID, Revision: 1}}}
	manifest.Presentation.Name = "Authoring fixture"
	input := client.PublicationInput{ExpectedRevision: 0, Enabled: true, Manifest: manifest}
	initialFile := write("initial-publication.json", input)
	var initialPreview client.PublicationPreview
	decode(t, call("publication", "preview", "--file", initialFile), &initialPreview)
	call("publication", "apply", "--file", initialFile, "--preview-sha256", initialPreview.SHA256, "--key", "authoring-publish-initial")

	// CLI and SDK MCP transmit only the changed fields. Exact replay remains
	// recoverable after later changes; stale requests cannot erase their peers.
	patch := write("next-change.json", map[string]any{"next": "One new milestone"})
	args := []string{"map", "update", created.ID, "--file", patch, "--expect-revision", "1", "--key", "authoring-map-update"}
	firstReceipt := call(args...)
	if !bytes.Equal(firstReceipt, call(args...)) || len(firstReceipt) > 512 {
		t.Fatal("compact map update replay failed")
	}
	var updated client.ProjectMapEntry
	decode(t, call("map", "read", created.ID), &updated)
	if updated.Revision != 2 || updated.Description != entry.Description || updated.Where != entry.Where || updated.Next != "One new milestone" || !reflect.DeepEqual(updated.Phases, entry.Phases) || updated.Kind != entry.Kind {
		t.Fatal("one-field edit did not preserve omitted source")
	}
	refuse("map", "update", created.ID, "--file", patch, "--expect-revision", "1", "--key", "authoring-stale")
	refuse("map", "update", created.ID, "--file", write("phase-loss.json", map[string]any{"phases": []any{}}), "--expect-revision", "2", "--key", "authoring-phase-loss")
	command := exec.CommandContext(ctx, fcmcp, base...)
	command.Env = env
	mcpStderr := &boundedOutput{}
	command.Stderr = mcpStderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "text-authoring-smoke", Version: "1"}, nil)
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual authoring MCP initialization failed")
	}
	var closeOnce sync.Once
	closeDone := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		closeOnce.Do(func() { go func() { closeErr = session.Close(); close(closeDone) }() })
		select {
		case <-closeDone:
			if closeErr != nil || mcpStderr.len() != 0 || command.ProcessState == nil || !command.ProcessState.Success() {
				t.Error("actual authoring MCP did not join cleanly")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-closeDone:
			case <-time.After(time.Second):
				t.Error("owned authoring MCP did not join after kill")
			}
			t.Error("authoring MCP shutdown exceeded bound")
		}
	}
	t.Cleanup(closeMCP)
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	result, err := session.CallTool(bounded, &mcp.CallToolParams{Name: "map_entry_update", Arguments: map[string]any{"entry_id": created.ID, "expected_revision": 2, "changes": map[string]any{"state": "Ready"}, "idempotency_key": "authoring-mcp-state"}})
	stop()
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatal("actual authoring MCP update failed")
	}
	mcpBody, err := json.Marshal(result.StructuredContent)
	text, ok := result.Content[0].(*mcp.TextContent)
	if err != nil || !ok || !jsonEqual(mcpBody, []byte(text.Text)) || len(mcpBody) > 512 {
		t.Fatal("authoring MCP receipt is not compact matching JSON")
	}
	closeMCP()
	call("map", "update", created.ID, "--file", write("clear-next.json", map[string]any{"next": ""}), "--expect-revision", "3", "--key", "authoring-map-clear")
	decode(t, call("map", "read", created.ID), &updated)
	if updated.Revision != 4 || updated.State != "Ready" || updated.Next != "" || updated.Description != entry.Description || updated.Where != entry.Where || !reflect.DeepEqual(updated.Phases, entry.Phases) {
		t.Fatal("MCP update and explicit clear lost omitted content")
	}
	if !bytes.Equal(firstReceipt, call(args...)) {
		t.Fatal("historical partial edit replay changed receipt")
	}
	call("jobs", "update", job.ID, "--expect-revision", "1", "--brief", "PRIVATE_BRIEF_NEW", "--key", "authoring-private-job")
	call("jobs", "summary-set", job.ID, "--expect-revision", "2", "--expect-summary-revision", "1", "--title", "UNSELECTED_SUMMARY_TWO", "--body", "Unselected later audience prose", "--key", "authoring-summary-two")

	// The small plan preserves unrelated approved editorial cards and exact
	// reference revisions. Preparation previews but does not publish or silently
	// pick later private/source revisions.
	plan := write("publication-plan.json", map[string]any{"expected_revision": 1, "description": "Changed introduction only", "worker_refs": map[string]any{"upsert": []any{map[string]any{"key": "selected_worker", "worker_id": worker, "label": "Builder"}}}})
	prepared := filepath.Join(private, "prepared-publication.json")
	var preparation struct {
		ExpectedRevision int64  `json:"expected_revision"`
		SHA256           string `json:"preview_sha256"`
		PreparedFile     string `json:"prepared_file"`
	}
	prepareReceipt := call("publication", "prepare", "--file", plan, "--output", prepared)
	decode(t, prepareReceipt, &preparation)
	if preparation.ExpectedRevision != 1 || len(preparation.SHA256) != 64 || preparation.PreparedFile != prepared || len(prepareReceipt) > 2048 {
		t.Fatal("publication preparation receipt invalid")
	}
	approvedRaw, err := os.ReadFile(prepared)
	info, statErr := os.Stat(prepared)
	if err != nil || statErr != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("prepared approved file is not private")
	}
	approved, err := client.DecodePublicationInput(approvedRaw)
	if err != nil || approved.ExpectedRevision != 1 || !approved.Enabled || approved.Manifest.Slug != "authoring-smoke" || approved.Manifest.Description != "Changed introduction only" || !reflect.DeepEqual(approved.Manifest.ProjectMap, initialPreview.Manifest.ProjectMap) || !reflect.DeepEqual(approved.Manifest.JobRefs, initialPreview.Manifest.JobRefs) || !reflect.DeepEqual(approved.Manifest.MapRefs, initialPreview.Manifest.MapRefs) || len(approved.Manifest.WorkerRefs) != 1 {
		t.Fatal("preparation changed unrelated approved fields or pinned selections")
	}
	var config client.PublicationConfiguration
	decode(t, call("publication", "read"), &config)
	if config.Revision != 1 || config.Manifest.Description != "Original project introduction" {
		t.Fatal("prepare published before explicit apply")
	}
	refuse("publication", "prepare", "--file", plan, "--output", prepared)
	preserved, _ := os.ReadFile(prepared)
	if !bytes.Equal(approvedRaw, preserved) {
		t.Fatal("prepare overwrote a retained approved artifact")
	}
	applyArgs := []string{"publication", "apply", "--file", prepared, "--preview-sha256", preparation.SHA256, "--key", "authoring-prepared-apply"}
	// Discard the real executable's first successful receipt, then retry exactly
	// from the retained artifact/key/hash without preparing or publishing again.
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	lost := exec.CommandContext(bounded, fc, append(append([]string{}, base...), applyArgs...)...)
	lost.Env, lost.Stdout = env, io.Discard
	lostDiagnostic := &boundedOutput{}
	lost.Stderr = lostDiagnostic
	lostErr := lost.Run()
	stop()
	if lostErr != nil || lostDiagnostic.len() != 0 {
		t.Fatal("discarded-receipt apply did not finish")
	}
	recovered := call(applyArgs...)
	if !bytes.Equal(recovered, call(applyArgs...)) {
		t.Fatal("prepared publication replay changed receipt")
	}
	decode(t, call("publication", "read"), &config)
	if config.Revision != 2 {
		t.Fatal("replayed preparation created another publication revision")
	}
	staleOutput := filepath.Join(private, "stale-preparation.json")
	refuse("publication", "prepare", "--file", plan, "--output", staleOutput)
	if _, statErr := os.Stat(staleOutput); !os.IsNotExist(statErr) {
		t.Fatal("stale preparation created an approval artifact")
	}
	retained, _ := os.ReadFile(prepared)
	if !bytes.Equal(retained, approvedRaw) {
		t.Fatal("apply/replay rewrote the retained approval artifact")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+daemon.address+"/api/v1/public/projects/authoring-smoke", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("anonymous selected publication unavailable")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte("APPROVED_SUMMARY_ONE")) || !bytes.Contains(body, []byte("Unrelated editorial card")) || !bytes.Contains(body, []byte("Changed introduction only")) || !bytes.Contains(body, []byte("UNCHANGED_DESCRIPTION")) {
		t.Fatal("prepared publication omitted approved content")
	}
	for _, forbidden := range []string{"PRIVATE_BRIEF_ORIGINAL", "PRIVATE_BRIEF_NEW", "UNSELECTED_SUMMARY_TWO", "UNCHANGED_WHERE", project, worker, job.ID, created.ID, strings.TrimSpace(string(token))} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatal("anonymous publication leaked private or unselected content")
		}
	}
	if external.Load() != 0 || patchCalls.Load() < 6 {
		t.Fatal("authoring workload made external traffic or omitted actual PATCH calls")
	}
	t.Logf("packaged text authoring passed: elapsed=%s map_patch_calls=%d map_patch_bytes=%d unchanged_description_bytes=%d unchanged_where_bytes=%d prepare_receipt_bytes=%d publication_revision=%s discarded_receipt_replay=true official_sdk_mcp=true external=0 owned_processes=0", time.Since(started).Round(time.Millisecond), patchCalls.Load(), patchBytes.Load(), len(entry.Description), len(entry.Where), len(prepareReceipt), strconv.FormatInt(config.Revision, 10))
}
