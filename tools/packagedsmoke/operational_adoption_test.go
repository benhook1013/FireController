//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
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

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

// Actual packaged offline conversion and administrator adoption; source history
// and execution authority remain separate. No provider or fake native work runs.
func TestPackagedOperationalAdoptionSmoke(t *testing.T) {
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
	for _, name := range []string{"fc", "fcd", "legacyimport"} {
		path := filepath.Join(binDir, name+suffix)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("packaged %s missing; build optional migration binaries before adoption-smoke", name)
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
	seedOperationalAdoptionQueue(t, ctx, data, project, builder, strings.TrimSpace(string(admin)))
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
		allowed := r.Method == "GET" || r.Method == "POST" && (r.URL.Path == prefix+"/jobs" || r.URL.Path == prefix+"/messages" || strings.HasPrefix(r.URL.Path, prefix+"/messages/") || strings.HasPrefix(r.URL.Path, prefix+"/reviews/rounds/") || r.URL.Path == prefix+"/coordination/imports/preview" || r.URL.Path == prefix+"/coordination/imports/apply")
		if !allowed {
			denied.Add(1)
			w.WriteHeader(403)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := map[string]string{"builder": filepath.Join(private, "adoption-builder.json"), "inspector": filepath.Join(private, "adoption-inspector.json")}
	calls, utilities := 0, 0
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
	utility := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, binaries["legacyimport"], env, args...)
		utilities++
		safe(body)
		if len(body) > 4096 {
			t.Fatal("utility summary too large")
		}
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
	var unrelated client.JobReceipt
	decode(t, call("builder", "jobs", "create", "--title", "Existing native job", "--brief", "Unchanged native brief", "--worker", builder, "--key", "adoption-unrelated-job"), &unrelated)
	var unrelatedMessage client.MessageReceipt
	decode(t, call("builder", "inbox", "send", "--to", inspector, "--body", "Existing native message", "--key", "adoption-unrelated-message"), &unrelatedMessage)
	unrelatedJobBefore := call("builder", "jobs", "read", unrelated.ID)
	unrelatedThreadBefore := call("inspector", "inbox", "thread", unrelatedMessage.ConversationID)
	initial := readUpgradeState(t, ctx, data)
	zero := int64(0)
	preparedControl := roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "packaged-sealed-controls", SourceKey: "pr-2-cli", SourceSHA256: strings.Repeat("a", 64), MinimumRemaining: 1, MaximumRemaining: 4, InitialDry: 2, Taper: 3}
	controlFile := writeJSON("control.json", preparedControl)
	controlHash := binaryHash(t, controlFile)
	var preview client.ReviewRoundAdoptionPreview
	decode(t, call("builder", "reviews", "rounds", "adopt", "preview", "--file", controlFile), &preview)
	if !reflect.DeepEqual(initial, readUpgradeState(t, ctx, data)) {
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
	if replaced.Revision != 2 || replaced.Epoch != 2 || replaced.Minimum != 0 || replaced.Maximum != 3 || replaced.Closed || replaced.Used != 0 || replaced.Dry != 0 || replaced.InitialDry != 0 || replaced.InitiallyTapered || replaced.OriginSource != "" || replaced.OriginKey != "" || replaced.OriginSHA256 != "" {
		t.Fatal("ordinary grant retained migrated carry")
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
		{"3", "cli", "sticky", roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 3, Channel: "cli", Source: "packaged-sealed-controls", SourceKey: "pr-3-cli", SourceSHA256: strings.Repeat("c", 64), MinimumRemaining: 2, MaximumRemaining: 4, Taper: 3, Tapered: true}},
		{"3", "hosted", "closed", roundadoptionmodel.Prepared{Version: 1, RepositoryID: 991, ExpectedRevision: &zero, PullNumber: 3, Channel: "hosted", Source: "packaged-sealed-controls", SourceKey: "pr-3-hosted", SourceSHA256: strings.Repeat("d", 64), MaximumRemaining: 2, Taper: 1, Closed: true}},
	}
	inputHashes := map[string][32]byte{controlFile: controlHash, adminFile: adminHash}
	for path, hash := range profileHashes {
		inputHashes[path] = hash
	}
	for _, variant := range variants {
		file := writeJSON("control-"+variant.name+".json", variant.control)
		inputHashes[file] = binaryHash(t, file)
		decode(t, call("builder", "reviews", "rounds", "adopt", "preview", "--file", file), &preview)
		call("builder", "reviews", "rounds", "adopt", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "adoption-"+variant.name)
		actual := readRound(variant.pull, variant.channel)
		if actual.Used != 0 || actual.Maximum != variant.control.MaximumRemaining || actual.Minimum != variant.control.MinimumRemaining || actual.Closed != variant.control.Closed || actual.InitiallyTapered != variant.control.Tapered || actual.Tapered {
			t.Fatal("zero/sticky/closed control changed operational meaning")
		}
	}
	beforeMessages := readUpgradeState(t, ctx, data)
	snapshot := operationalLegacyMessageSnapshot(t, ctx, private)
	sourceHash := binaryHash(t, snapshot)
	sourceDigest := hex.EncodeToString(sourceHash[:])
	selection := writeJSON("message-selection.json", map[string]any{"version": 1, "repository_id": 991, "expected_revision": 0, "source": "packaged-sealed-messages", "message_ids": []string{"legacy-reply", "legacy-root"}, "workers": map[string]string{"Old builder": builder, "Old inspector": inspector}, "association_policy": "archive"})
	inputHashes[snapshot], inputHashes[selection] = sourceHash, binaryHash(t, selection)
	prepared := filepath.Join(private, "prepared-messages.json")
	var summaryWire struct {
		Kind                 string `json:"kind"`
		SourceSHA256         string `json:"source_sha256"`
		Messages             int    `json:"messages"`
		Replies              int    `json:"replies"`
		ArchivedAssociations int    `json:"archived_associations"`
	}
	summaryBytes := utility("prepare-messages", "--snapshot", snapshot, "--selection", selection, "--output", prepared)
	decode(t, summaryBytes, &summaryWire)
	if summaryWire.Kind != "prepared_message_import" || summaryWire.SourceSHA256 != sourceDigest || summaryWire.Messages != 2 || summaryWire.Replies != 1 || summaryWire.ArchivedAssociations != 3 || bytes.Contains(summaryBytes, []byte("Historical")) {
		t.Fatal("message preparation selected wrong scope or disclosed body")
	}
	inputHashes[prepared] = binaryHash(t, prepared)
	if !bytes.Equal(summaryBytes, utility("prepare-messages", "--snapshot", snapshot, "--selection", selection, "--output", prepared)) {
		t.Fatal("repeat preparation changed selected summary")
	}
	raw, err := os.ReadFile(prepared)
	if err != nil {
		t.Fatal("prepared source unavailable")
	}
	var batch coordimportmodel.Batch
	decode(t, raw, &batch)
	if len(batch.Jobs) != 0 || len(batch.Messages) != 2 || batch.SourceSHA256 != sourceDigest || batch.Messages[0].SourceKey != "legacy-root" || batch.Messages[1].ReplyToSourceKey != "legacy-root" || batch.Messages[0].SeenAt == "" || batch.Messages[1].SeenAt != "" {
		t.Fatal("prepared reply/order/handling lost original facts")
	}
	var messagePreview client.CoordinationImportPreview
	decode(t, call("builder", "import", "preview", "--file", prepared), &messagePreview)
	if messagePreview.Counts.NewMessages != 2 || messagePreview.Counts.NewJobs != 0 || !reflect.DeepEqual(beforeMessages, readUpgradeState(t, ctx, data)) {
		t.Fatal("message preview wrote state or widened selection")
	}
	messageApply := []string{"import", "apply", "--file", prepared, "--preview", messagePreview.PreviewSHA256, "--key", "adoption-message-original"}
	importedBytes := call("builder", messageApply...)
	var imported client.CoordinationImportReceipt
	decode(t, importedBytes, &imported)
	if imported.Revision != 1 || imported.Outcome != "imported" || imported.Counts.NewMessages != 2 || len(imported.MessageMappings) != 2 || len(imported.Mappings) != 0 {
		t.Fatal("message mappings not compact selected scope")
	}
	mappings := map[string]coordimportmodel.MessageMapping{}
	for _, mapping := range imported.MessageMappings {
		mappings[mapping.SourceKey] = mapping
	}
	root, reply := mappings["legacy-root"], mappings["legacy-reply"]
	if root.MessageID == "" || reply.MessageID == "" || root.ConversationID != reply.ConversationID {
		t.Fatal("imported replies lost conversation identity")
	}
	var thread client.ConversationPage
	decode(t, call("builder", "inbox", "thread", root.ConversationID), &thread)
	if thread.HasMore || len(thread.Items) != 2 || thread.RootMessageID != root.MessageID || thread.Items[0].ID != root.MessageID || thread.Items[0].Body != batch.Messages[0].Body || thread.Items[0].CreatedAt != batch.Messages[0].CreatedAt || thread.Items[1].ID != reply.MessageID || thread.Items[1].ReplyToMessageID != root.MessageID || thread.Items[1].Body != batch.Messages[1].Body || thread.Items[1].CreatedAt != batch.Messages[1].CreatedAt {
		t.Fatal("historical thread lost exact times/text/reply")
	}
	var rootInbox, pending client.Page[client.InboxItem]
	decode(t, call("builder", "inbox", "list"), &rootInbox)
	if len(rootInbox.Items) != 1 || rootInbox.Items[0].ID != root.MessageID || !rootInbox.Items[0].Seen || !rootInbox.Items[0].Acknowledged {
		t.Fatal("known source handling was not retained")
	}
	decode(t, call("inspector", "inbox", "list", "--unread", "--unacknowledged"), &pending)
	found := false
	for _, item := range pending.Items {
		if item.ID == reply.MessageID {
			found = true
			if item.Seen || item.Acknowledged {
				t.Fatal("reading pending historical message handled it")
			}
		}
	}
	if !found {
		t.Fatal("pending historical reply absent from recipient inbox")
	}
	ackArgs := []string{"inbox", "ack", reply.MessageID, "--key", "adoption-later-ack"}
	ackBytes := call("inspector", ackArgs...)
	if !bytes.Equal(importedBytes, call("builder", messageApply...)) {
		t.Fatal("message replay changed original receipt")
	}
	decode(t, call("inspector", "inbox", "list"), &pending)
	found = false
	for _, item := range pending.Items {
		if item.ID == reply.MessageID {
			found = true
			if !item.Seen || !item.Acknowledged {
				t.Fatal("historical retry erased later native acknowledgment")
			}
		}
	}
	if !found {
		t.Fatal("acknowledged reply disappeared")
	}
	if !bytes.Equal(unrelatedJobBefore, call("builder", "jobs", "read", unrelated.ID)) || !bytes.Equal(unrelatedThreadBefore, call("inspector", "inbox", "thread", unrelatedMessage.ConversationID)) {
		t.Fatal("adoption touched unrelated native coordination")
	}
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	changed := map[string]bool{"review_round_controls": true, "review_round_changes": true, "review_round_adoptions": true, "coordination_import_state": true, "coordination_import_messages": true, "coordination_import_batches": true, "inbox_conversations": true, "inbox_messages": true, "inbox_recipient_state": true, "events": true, "receipts": true, "sqlite_sequence": true}
	if initial.version != retained.version || !reflect.DeepEqual(initial.metadata, retained.metadata) || len(initial.tables) != len(retained.tables) {
		t.Fatal("adoption changed native identity/schema")
	}
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
	if retained.counts["review_round_adoptions"] != 4 || retained.counts["coordination_import_messages"] != 2 || retained.counts["coordination_import_batches"] != 1 || retained.counts["inbox_messages"] != initial.counts["inbox_messages"]+2 {
		t.Fatal("adoption widened or duplicated retained selection")
	}
	operationalAdoptionProvenance(t, ctx, data, project, sourceDigest, batch.Messages)
	restarted := startDaemon(t, ctx, binaries["fcd"], data, env)
	if restarted.instance != bootstrap.instance {
		t.Fatal("restart changed native identity")
	}
	next, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	if !bytes.Equal(adoptedBytes, call("builder", applyControl...)) || readRound("2", "cli") != replaced || !bytes.Equal(importedBytes, call("builder", messageApply...)) || !bytes.Equal(ackBytes, call("inspector", ackArgs...)) {
		t.Fatal("restart lost original receipts or overwritten state")
	}
	decode(t, call("inspector", "inbox", "list"), &pending)
	found = false
	for _, item := range pending.Items {
		if item.ID == reply.MessageID {
			found = true
			if !item.Acknowledged || !item.Seen {
				t.Fatal("restart lost native handling")
			}
		}
	}
	if !found {
		t.Fatal("restart lost imported reply")
	}
	restarted.stop(t)
	if !reflect.DeepEqual(retained, readUpgradeState(t, ctx, data)) {
		t.Fatal("restart/exact replay changed durable rows")
	}
	for path, hash := range inputHashes {
		if binaryHash(t, path) != hash {
			t.Fatal("approved private input changed")
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(snapshot + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("offline preparation created source sidecar")
		}
	}
	if external.Load() != 0 || denied.Load() != 0 {
		t.Fatal("adoption attempted provider or unsupported HTTP")
	}
	t.Logf("packaged operational adoption passed: total=%s cli_calls=%d utility_calls=%d max_cli=%s api_requests=%d adopted_controls=4 imported_messages=2 selected_replies=1 output_loss=1 unrelated_tables_preserved=%d external_http=0 native_provider_operations=0 owned_processes=0; synthetic offline scope only", time.Since(started).Round(time.Millisecond), calls, utilities, maxCLI.Round(time.Millisecond), requests.Load(), len(initial.tables)-len(changed))
}

// Trusted, stopped authenticated configuration, with no execution or raw SQL
// control/evidence injection. The actual daemon later starts with providers off.
func seedOperationalAdoptionQueue(t *testing.T, ctx context.Context, data, project, worker, admin string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("trusted adoption setup unavailable")
	}
	defer store.Close()
	engine, err := reviewexec.New(store, []reviewexec.Capability{{ID: "synthetic-adoption", Provider: "coderabbit", Account: "synthetic-adoption", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("e", 64)}})
	if err != nil {
		t.Fatal("trusted configuration engine unavailable")
	}
	mux := http.NewServeMux()
	engine.Register(mux)
	raw, _ := json.Marshal(map[string]any{"expected_revision": 0, "capability_id": "synthetic-adoption", "mode": "independent", "queue": []int64{2, 3}, "channels": []string{"cli", "hosted"}, "paused": false})
	r := httptest.NewRequest("POST", "/api/v1/projects/"+project+"/reviews/execution/configuration", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+admin)
	r.Header.Set("X-FC-Instance", store.InstanceID())
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("Idempotency-Key", "adoption-trusted-configuration")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var receipt reviewexec.Receipt
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID != project || receipt.State != "configured" || receipt.Revision != 1 {
		t.Fatalf("trusted queue configuration receipt invalid status=%d", w.Code)
	}
}

func operationalLegacyMessageSnapshot(t *testing.T, ctx context.Context, private string) string {
	t.Helper()
	path := coordinationLegacySnapshot(t, ctx, private)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("synthetic source messages unavailable")
	}
	defer db.Close()
	// SQL writes affect only the exclusively owned synthetic source format.
	for _, row := range []struct {
		id, recipient, body, author string
		job, pr, reply, seen, ack   any
		at                          string
	}{
		{"legacy-root", "Old builder", "Historical root <literal>\nexact text", "Old inspector", "source-selected-job", 2, nil, "2024-02-01T02:02:03Z", "2024-02-01T03:02:03Z", "2024-02-01T01:02:03.123456789Z"},
		{"legacy-reply", "Old inspector", "Historical reply <literal>\nsecond line", "Old builder", nil, 3, "legacy-root", nil, nil, "2024-02-02T00:00:00+01:00"},
		{"legacy-unselected", "Old builder", "Unselected source body", "Old inspector", nil, nil, nil, nil, nil, "2024-02-03T01:02:03Z"},
	} {
		if _, err = db.ExecContext(ctx, `INSERT INTO inbox_messages(id,recipient,body,author,job,pr,reply_to,created_at,seen_at,acknowledged_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, row.id, row.recipient, row.body, row.author, row.job, row.pr, row.reply, row.at, row.seen, row.ack); err != nil {
			t.Fatal("synthetic source message seed failed")
		}
	}
	if db.Close() != nil {
		t.Fatal("synthetic source seal failed")
	}
	return path
}

// Read-only private checks retain original handling timestamps in provenance,
// while a later native acknowledgment changes only native recipient flags.
func operationalAdoptionProvenance(t *testing.T, ctx context.Context, data, project, sourceHash string, want []coordimportmodel.PreparedMessage) {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(data, "firecontroller.sqlite")), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("private adoption provenance unavailable")
	}
	defer db.Close()
	for _, item := range want {
		var hash, body string
		if db.QueryRowContext(ctx, `SELECT source_sha256,item_json FROM coordination_import_messages WHERE project_id=? AND source='packaged-sealed-messages' AND source_key=?`, project, item.SourceKey).Scan(&hash, &body) != nil {
			t.Fatal("message source mapping absent")
		}
		var original coordimportmodel.PreparedMessage
		if hash != sourceHash || json.Unmarshal([]byte(body), &original) != nil || original != item {
			t.Fatal("message provenance lost original handling or text")
		}
	}
	var sent int
	if db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_type='message.sent'`).Scan(&sent) != nil || sent != 1 {
		t.Fatal("historical import replayed send events")
	}
}
