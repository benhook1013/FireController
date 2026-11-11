//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Actual supplied binaries adopt synthetic prepared controls and source holds.
// This does not qualify live source completeness or provider dispatch guards.
func TestPackagedCarriedAdoptionSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private adoption fixture unavailable")
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
		path := filepath.Join(binDir, name+suffix)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("packaged %s missing; run make build before carried adoption smoke", name)
		}
		if _, err = exec.LookPath(path); err != nil {
			t.Fatalf("packaged %s not executable", name)
		}
		binaries[name], hashes[name] = path, binaryHash(t, path)
		t.Logf("adoption input %s sha256=%x", name, hashes[name])
	}
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s changed", name)
			}
		}
	}()
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("bootstrap authority unavailable")
	}
	bootstrap.stop(t)
	project, builder, inspector, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	forward.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	prefix := "/api/v1/projects/" + project
	var requests, denied atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		allowed := r.Method == "GET" || r.Method == "POST" && (strings.HasPrefix(r.URL.Path, prefix+"/reviews/rounds/") || strings.HasPrefix(r.URL.Path, prefix+"/reviews/adoption-obligations/"))
		if !allowed {
			denied.Add(1)
			w.WriteHeader(403)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := map[string]string{"builder": filepath.Join(private, "adoption-builder.json"), "inspector": filepath.Join(private, "adoption-inspector.json")}
	calls := 0
	var maxCLI time.Duration
	safe := func(raw []byte) {
		t.Helper()
		if bytes.Contains(raw, bytes.TrimSpace(admin)) {
			t.Fatal("transport exposed credential")
		}
	}
	commandArgs := func(profile string, args []string) []string {
		return append([]string{"--profiles-file", profiles[profile], "--profile", profile}, args...)
	}
	call := func(profile string, args ...string) json.RawMessage {
		t.Helper()
		defer func() {
			if t.Failed() {
				t.Logf("adoption CLI failure at invocation=%d profile=%s", calls+1, profile)
			}
		}()
		start := time.Now()
		body := runCLI(t, ctx, binaries["fc"], env, commandArgs(profile, args)...)
		calls++
		if elapsed := time.Since(start); elapsed > maxCLI {
			maxCLI = elapsed
		}
		safe(body)
		return body
	}
	writeJSON := func(name string, value any) string {
		t.Helper()
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal("private JSON unavailable")
		}
		path := filepath.Join(private, name)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("private JSON publication failed")
		}
		return path
	}
	for _, binding := range []struct{ name, worker string }{{"builder", builder}, {"inspector", inspector}} {
		call(binding.name, "profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", binding.worker, "--credential-file", adminFile)
	}
	adminHash := binaryHash(t, adminFile)
	profileHashes := map[string][32]byte{}
	for _, path := range profiles {
		profileHashes[path] = binaryHash(t, path)
	}
	initial := readUpgradeState(t, ctx, data)
	zero := int64(0)
	holds := adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "packaged-sealed-controls", RepositoryID: 991, SnapshotSHA256: strings.Repeat("f", 64)}, ExpectedRevision: &zero,
		Obligations: []adoptionholdsmodel.Obligation{
			{SourceKey: "pending-finding", Kind: "finding", AffectedPullNumbers: []int64{3}, Summary: "Private original finding", Evidence: []string{}, OriginalStatus: "pending"},
			{SourceKey: "pending-route", Kind: "route", AffectedPullNumbers: []int64{3}, Summary: "Private original routed correction", Evidence: []string{"source:route"}, OriginalStatus: "open"},
		}}
	holdFile := writeJSON("obligations.json", holds)
	var holdPreview client.AdoptionObligationPreview
	decode(t, call("builder", "reviews", "obligations", "adopt", "preview", "--file", holdFile), &holdPreview)
	if holdPreview.Selected != 2 || holdPreview.New != 2 || !reflect.DeepEqual(initial, readUpgradeState(t, ctx, data)) {
		t.Fatal("obligation preview changed state or selection")
	}
	holdApply := []string{"reviews", "obligations", "adopt", "apply", "--file", holdFile, "--preview", holdPreview.PreviewSHA256, "--key", "adoption-holds-original"}
	holdReceipt := call("builder", holdApply...)
	var held client.AdoptionObligationReceipt
	decode(t, holdReceipt, &held)
	if held.New != 2 || held.Revision != 1 || !bytes.Equal(holdReceipt, call("builder", holdApply...)) {
		t.Fatal("obligation apply lost source revision or exact replay")
	}
	var holdPage client.AdoptionObligationPage
	compactHolds := call("builder", "reviews", "obligations", "list", "--pr", "3", "--status", "open")
	decode(t, compactHolds, &holdPage)
	if len(holdPage.Items) != 2 || holdPage.NextCursor != "" || bytes.Contains(compactHolds, []byte("Private original")) {
		t.Fatal("compact obligations lost selected holds or expanded private text")
	}
	var resolvedID string
	for _, item := range holdPage.Items {
		if item.SourceKey == "pending-finding" {
			resolvedID = item.ID
		}
	}
	if resolvedID == "" {
		t.Fatal("finding mapping unavailable")
	}
	var original client.AdoptionObligation
	decode(t, call("builder", "reviews", "obligations", "read", resolvedID), &original)
	if original.Original.Summary != holds.Obligations[0].Summary || original.Original.Evidence == nil || len(original.Original.Evidence) != 0 || original.Status != "open" {
		t.Fatal("deliberate obligation detail lost original facts")
	}
	resolveArgs := []string{"reviews", "obligations", "resolve", resolvedID, "--expected-revision", "1", "--reason", "Synthetic correction verified", "--evidence", "proof:synthetic-correction", "--key", "adoption-hold-resolution"}
	resolvedReceipt := call("builder", resolveArgs...)
	if !bytes.Equal(holdReceipt, call("builder", holdApply...)) {
		t.Fatal("original import receipt changed after resolution")
	}
	decode(t, call("builder", "reviews", "obligations", "list", "--pr", "3", "--status", "open"), &holdPage)
	if len(holdPage.Items) != 1 || holdPage.Items[0].SourceKey != "pending-route" {
		t.Fatal("reimport reopened a resolved finding or lost its unresolved route")
	}
	preparedControl := roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "packaged-sealed-controls", SourceKey: "pr-2-cli", SourceSHA256: strings.Repeat("a", 64), MinimumRemaining: 1, MaximumRemaining: 4, InitialDry: 2, Taper: 3}
	controlFile := writeJSON("control.json", preparedControl)
	controlHash := binaryHash(t, controlFile)
	beforeRoundPreview := readUpgradeState(t, ctx, data)
	var preview client.ReviewRoundAdoptionPreview
	decode(t, call("builder", "reviews", "rounds", "adopt", "preview", "--file", controlFile), &preview)
	if !reflect.DeepEqual(beforeRoundPreview, readUpgradeState(t, ctx, data)) {
		t.Fatal("round preview changed native state")
	}
	applyControl := []string{"reviews", "rounds", "adopt", "apply", "--file", controlFile, "--preview", preview.PreviewSHA256, "--key", "adoption-control-original"}
	// Real committed action with a read-only stdout descriptor; recover only the
	// original key/body/preview, then prove it cannot undo a later ordinary grant.
	readOnly, e := os.Open(os.DevNull)
	if e != nil {
		t.Fatal("lost stdout fixture unavailable")
	}
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	lost := exec.CommandContext(bounded, binaries["fc"], commandArgs("builder", applyControl)...)
	lost.Env, lost.Stdout, lost.WaitDelay = env, readOnly, time.Second
	lostErrors := &boundedOutput{}
	lost.Stderr = lostErrors
	start := time.Now()
	runErr := lost.Run()
	stop()
	readOnly.Close()
	calls++
	if elapsed := time.Since(start); elapsed > maxCLI {
		maxCLI = elapsed
	}
	var exit *exec.ExitError
	var failure struct {
		Error struct {
			Code           string `json:"code"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
		} `json:"error"`
	}
	if !errors.As(runErr, &exit) || exit.ExitCode() != cli.ExitConfig || lostErrors.len() > 4096 || json.Unmarshal(lostErrors.bytes(), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.OutcomeUnknown {
		t.Fatal("adoption lost stdout did not retain uncertain receipt")
	}
	safe(lostErrors.bytes())
	adoptedBytes := call("builder", applyControl...)
	var receipt client.ReviewRoundControlReceipt
	decode(t, adoptedBytes, &receipt)
	if receipt.Outcome != "adopted" || receipt.Revision != 1 || receipt.Epoch != 1 {
		t.Fatal("adoption receipt invalid")
	}
	readRound := func(pull, channel string) client.ReviewRoundControl {
		t.Helper()
		var result client.ReviewRoundControl
		decode(t, call("builder", "reviews", "rounds", "read", "--pr", pull, "--channel", channel), &result)
		return result
	}
	carried := readRound("2", "cli")
	if carried.Used != 0 || carried.Dry != 2 || carried.InitialDry != 2 || carried.Minimum != 1 || carried.Maximum != 4 || carried.Tapered || carried.OriginSource != preparedControl.Source || carried.OriginKey != preparedControl.SourceKey || carried.OriginSHA256 != preparedControl.SourceSHA256 {
		t.Fatal("partial dry or provenance was not retained")
	}
	call("builder", "reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--max", "3", "--key", "adoption-new-grant")
	replaced := readRound("2", "cli")
	if replaced.Revision != 2 || replaced.Epoch != carried.Epoch || replaced.Minimum != 0 || replaced.Maximum != 3 || replaced.Closed || replaced.Used != 0 || replaced.Dry != carried.Dry || replaced.InitialDry != carried.InitialDry || replaced.InitiallyTapered != carried.InitiallyTapered || replaced.Tapered || replaced.Taper != carried.Taper || replaced.OriginSource != carried.OriginSource || replaced.OriginKey != carried.OriginKey || replaced.OriginSHA256 != carried.OriginSHA256 {
		t.Fatal("ordinary grant changed earned carry or ignored additional limits")
	}
	if !bytes.Equal(adoptedBytes, call("builder", applyControl...)) || readRound("2", "cli") != replaced {
		t.Fatal("old adoption retry overwrote later grant")
	}
	// Three other explicitly configured channel slots: zero remaining allowance,
	// sticky historical latch with unmet minimum, and durable closure.
	variants := []struct {
		pull, channel, name string
		control             roundadoptionmodel.Prepared
	}{
		{"2", "hosted", "zero", roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 2, Channel: "hosted", Source: "packaged-sealed-controls", SourceKey: "pr-2-hosted", SourceSHA256: strings.Repeat("b", 64), Taper: 1}},
		{"3", "cli", "sticky-unbounded", roundadoptionmodel.Prepared{Version: 2, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 3, Channel: "cli", Source: "packaged-sealed-controls", SourceKey: "pr-3-cli", SourceSHA256: strings.Repeat("c", 64), MinimumRemaining: 2, MaximumUnbounded: true, Taper: 3, Tapered: true, Obligations: &roundadoptionmodel.ObligationsReference{Namespace: holds.Source.Namespace, SnapshotSHA256: holds.Source.SnapshotSHA256, Revision: 1}}},
		{"3", "hosted", "closed", roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 3, Channel: "hosted", Source: "packaged-sealed-controls", SourceKey: "pr-3-hosted", SourceSHA256: strings.Repeat("d", 64), MaximumRemaining: 2, Taper: 1, Closed: true}},
	}
	inputHashes := map[string][32]byte{controlFile: controlHash, adminFile: adminHash, holdFile: binaryHash(t, holdFile)}
	for path, hash := range profileHashes {
		inputHashes[path] = hash
	}
	for _, variant := range variants {
		file := writeJSON("control-"+variant.name+".json", variant.control)
		inputHashes[file] = binaryHash(t, file)
		decode(t, call("builder", "reviews", "rounds", "adopt", "preview", "--file", file), &preview)
		call("builder", "reviews", "rounds", "adopt", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "adoption-"+variant.name)
		actual := readRound(variant.pull, variant.channel)
		if actual.Used != 0 || actual.Maximum != variant.control.MaximumRemaining || actual.MaximumUnbounded != variant.control.MaximumUnbounded || actual.Minimum != variant.control.MinimumRemaining || actual.Closed != variant.control.Closed || actual.InitiallyTapered != variant.control.Tapered || actual.Tapered {
			t.Fatal("zero/sticky/closed control changed operational meaning")
		}
	}
	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profiles["builder"], "--profile", "builder")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-rounds", Version: "1"}, nil)
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual MCP initialization failed")
	}
	var closeOnce sync.Once
	done := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		closeOnce.Do(func() { go func() { closeErr = session.Close(); close(done) }() })
		select {
		case <-done:
			if closeErr != nil || command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
				t.Error("owned MCP shutdown failed")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("owned MCP did not join")
			}
			t.Error("MCP shutdown exceeded bound")
		}
	}
	t.Cleanup(closeMCP)
	mcpCalls := 0
	tool := func(name string, args any, wantError string) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		r, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		mcpCalls++
		if err != nil || r == nil || len(r.Content) != 1 || r.IsError != (wantError != "") {
			t.Fatalf("actual MCP tool %s failed", name)
		}
		body, err := json.Marshal(r.StructuredContent)
		if err != nil || len(body) > 4096 {
			t.Fatal("MCP compact output invalid")
		}
		safe(body)
		text, ok := r.Content[0].(*mcp.TextContent)
		if !ok || !jsonEqual(body, []byte(text.Text)) {
			t.Fatal("MCP compatibility text diverged")
		}
		if wantError != "" {
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decode(t, body, &failure)
			if failure.Error.Code != wantError {
				t.Fatal("MCP error classification incorrect")
			}
		}
		return body
	}
	// MCP uses the same prepared document and preserves the explicit absent cap.
	mcpPrepared := roundadoptionmodel.Prepared{Version: 2, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 4, Channel: "cli", Source: holds.Source.Namespace, SourceKey: "pr-4-cli", SourceSHA256: strings.Repeat("e", 64), MinimumRemaining: 2, MaximumUnbounded: true, Taper: 2, Obligations: &roundadoptionmodel.ObligationsReference{Namespace: holds.Source.Namespace, SnapshotSHA256: holds.Source.SnapshotSHA256, Revision: 1}}
	beforeMCPPreview := readUpgradeState(t, ctx, data)
	badLink := mcpPrepared
	badLink.Obligations = &roundadoptionmodel.ObligationsReference{Namespace: holds.Source.Namespace, SnapshotSHA256: strings.Repeat("0", 64), Revision: 1}
	tool("review_round_adoption_preview", map[string]any{"document": badLink}, "adoption_obligations_conflict")
	if !reflect.DeepEqual(beforeMCPPreview, readUpgradeState(t, ctx, data)) {
		t.Fatal("mismatched source preview changed durable state")
	}
	decode(t, tool("review_round_adoption_preview", map[string]any{"document": mcpPrepared}, ""), &preview)
	if !reflect.DeepEqual(beforeMCPPreview, readUpgradeState(t, ctx, data)) {
		t.Fatal("MCP preview wrote durable state")
	}
	mcpApply := map[string]any{"document": mcpPrepared, "preview_sha256": preview.PreviewSHA256, "idempotency_key": "mcp-carried-original"}
	mcpReceipt := tool("review_round_adoption_apply", mcpApply, "")
	mcpFile := writeJSON("mcp-control.json", mcpPrepared)
	mcpCLIApply := []string{"reviews", "rounds", "adopt", "apply", "--file", mcpFile, "--preview", preview.PreviewSHA256, "--key", "mcp-carried-original"}
	if !jsonEqual(mcpReceipt, call("builder", mcpCLIApply...)) {
		t.Fatal("cross-transport adoption replay changed receipt")
	}
	if got := readRound("4", "cli"); !got.MaximumUnbounded || got.Minimum != 2 || got.Used != 0 {
		t.Fatal("MCP lost minimum-only control")
	}
	// Moving a receiving route is not resolution and never rewrites its source.
	decode(t, tool("review_obligations_list", map[string]any{"pull_number": 3, "status": "open"}, ""), &holdPage)
	if len(holdPage.Items) != 1 {
		t.Fatal("unresolved route missing")
	}
	routeID := holdPage.Items[0].ID
	retargetArgs := map[string]any{"obligation_id": routeID, "expected_revision": 1, "target_pull_number": 4, "reason": "Synthetic receiving work moved", "idempotency_key": "carried-retarget"}
	retargetReceipt := tool("review_obligation_retarget", retargetArgs, "")
	var route client.AdoptionObligation
	decode(t, tool("review_obligation_read", map[string]any{"obligation_id": routeID}, ""), &route)
	if route.Status != "open" || route.Revision != 2 || route.Retarget == nil || !reflect.DeepEqual(route.Original, holds.Obligations[1]) || !reflect.DeepEqual(route.AffectedPullNumbers, []int64{3, 4}) {
		t.Fatal("retarget lost open source scope or original facts")
	}
	stale := map[string]any{"obligation_id": routeID, "expected_revision": 1, "target_pull_number": 0, "reason": "Stale change", "idempotency_key": "carried-stale"}
	beforeStale := readUpgradeState(t, ctx, data)
	tool("review_obligation_retarget", stale, "adoption_obligations_conflict")
	if !reflect.DeepEqual(beforeStale, readUpgradeState(t, ctx, data)) {
		t.Fatal("stale retarget changed durable state")
	}
	decode(t, call("builder", "reviews", "obligations", "list", "--pr", "4", "--status", "open"), &holdPage)
	if len(holdPage.Items) != 1 || holdPage.Items[0].ID != routeID {
		t.Fatal("retarget did not expose receiving hold")
	}
	decode(t, call("builder", "reviews", "obligations", "list", "--pr", "2", "--status", "open"), &holdPage)
	if len(holdPage.Items) != 0 {
		t.Fatal("carried route widened to unrelated PR")
	}
	closeMCP()
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	changed := map[string]bool{"review_round_controls": true, "review_round_changes": true, "review_round_adoptions": true, "review_adoption_sources": true, "review_adoption_imports": true, "review_adoption_obligations": true, "review_adoption_targets": true, "review_adoption_resolutions": true, "review_adoption_retargets": true, "events": true, "receipts": true, "sqlite_sequence": true}
	for name, hash := range initial.tables {
		if !changed[name] && (retained.tables[name] != hash || retained.counts[name] != initial.counts[name]) {
			t.Fatalf("adoption changed unrelated table %s", name)
		}
	}
	for _, name := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_evidence", "review_round_operation_bindings", "review_round_completions", "review_run_versions", "review_attempt_versions"} {
		if n, ok := retained.counts[name]; !ok || n != 0 {
			t.Fatalf("adoption manufactured native work in %s", name)
		}
	}
	if retained.counts["review_round_adoptions"] != 5 {
		t.Fatal("adoption duplicated or lost controls")
	}
	restarted := startDaemon(t, ctx, binaries["fcd"], data, env)
	if restarted.instance != bootstrap.instance {
		t.Fatal("restart changed native identity")
	}
	next, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	if !bytes.Equal(holdReceipt, call("builder", holdApply...)) || !bytes.Equal(resolvedReceipt, call("builder", resolveArgs...)) || !bytes.Equal(adoptedBytes, call("builder", applyControl...)) || !jsonEqual(mcpReceipt, call("builder", mcpCLIApply...)) || readRound("2", "cli") != replaced {
		t.Fatal("restart lost original receipts or overwrote later state")
	}
	if !jsonEqual(retargetReceipt, call("builder", "reviews", "obligations", "retarget", routeID, "--expected-revision", "1", "--pr", "4", "--reason", "Synthetic receiving work moved", "--key", "carried-retarget")) {
		t.Fatal("restart lost retarget receipt")
	}
	decode(t, call("builder", "reviews", "obligations", "read", resolvedID), &original)
	if original.Status != "resolved" || original.Resolution == nil || len(original.Resolution.Evidence) != 1 {
		t.Fatal("import replay reopened resolved source work")
	}
	decode(t, call("builder", "reviews", "obligations", "read", routeID), &route)
	if route.Status != "open" || route.Revision != 2 || !reflect.DeepEqual(route.AffectedPullNumbers, []int64{3, 4}) {
		t.Fatal("restart lost unresolved receiving hold")
	}
	restarted.stop(t)
	if !reflect.DeepEqual(retained, readUpgradeState(t, ctx, data)) {
		t.Fatal("restart/read/replay changed durable rows")
	}
	for path, hash := range inputHashes {
		if binaryHash(t, path) != hash {
			t.Fatal("private input changed")
		}
	}
	if external.Load() != 0 || denied.Load() != 0 {
		t.Fatal("adoption attempted provider or unsupported HTTP")
	}
	t.Logf("packaged carried adoption passed: total=%s cli_calls=%d mcp_calls=%d max_cli=%s controls=5 output_loss=1 external_http=0 native_provider_operations=0; synthetic prepared input only", time.Since(started).Round(time.Millisecond), calls, mcpCalls, maxCLI.Round(time.Millisecond))
}
