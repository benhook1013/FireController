//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

// Exercise already built programs; trusted repository bootstrap stays separate
// from prepared import, which must never collect provider history itself.
func TestPackagedMapImportSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private map import fixture unavailable")
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
	fc, fcd := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix)
	fcHash, fcdHash := binaryHash(t, fc), binaryHash(t, fcd)
	defer func() {
		if binaryHash(t, fc) != fcHash || binaryHash(t, fcd) != fcdHash {
			t.Error("packaged inputs changed")
		}
	}()
	var providers atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providers.Add(1); w.WriteHeader(502) }))
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
		t.Fatal("bootstrap token unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	health(t, ctx, daemon.address)
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var requests atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); proxy.ServeHTTP(w, r) }))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "admin.json")
	call := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, fc, env, append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)...)
		if bytes.Contains(body, bytes.TrimSpace(token)) {
			t.Fatal("packaged map operation disclosed a credential")
		}
		return body
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	// Every refusal below must happen before even the bound instance handshake.
	badFile := filepath.Join(private, "invalid.json")
	if err = os.WriteFile(badFile, []byte(`{"version":1,"version":1}`), 0600); err != nil {
		t.Fatal("invalid fixture unavailable")
	}
	refused := []string{badFile, private, filepath.Join(private, "missing.json")}
	if runtime.GOOS != "windows" {
		exposed := filepath.Join(private, "exposed.json")
		if err = os.WriteFile(exposed, []byte(`{}`), 0644); err != nil {
			t.Fatal("exposed fixture unavailable")
		}
		refused = append(refused, exposed)
	}
	if link := filepath.Join(private, "linked.json"); os.Symlink(badFile, link) == nil {
		refused = append(refused, link)
	}
	beforeRequests := requests.Load()
	for _, file := range refused {
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		command := exec.CommandContext(bounded, fc, "--profiles-file", filepath.Join(private, "absent-profiles.json"), "--profile", "admin", "map", "import-preview", "--file", file)
		command.Env = env
		command.WaitDelay = time.Second
		stdout, stderr := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = stdout, stderr
		runErr := command.Run()
		stop()
		if runErr == nil || stdout.len() != 0 || stderr.len() > 4096 || !json.Valid(stderr.bytes()) {
			t.Fatal("private file refusal failed")
		}
		var diagnostic struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decode(t, stderr.bytes(), &diagnostic)
		if diagnostic.Error.Code != "invalid_import_file" && diagnostic.Error.Code != "validation_failed" {
			t.Fatal("invalid file loaded profiles instead of local admission")
		}
		if bytes.Contains(stderr.bytes(), []byte(file)) || bytes.Contains(stderr.bytes(), bytes.TrimSpace(token)) {
			t.Fatal("private file diagnostic disclosed input")
		}
	}
	if requests.Load() != beforeRequests {
		t.Fatal("invalid private input contacted service")
	}
	entry := client.ProjectMapEditable{Kind: "workstream", Title: "Approved first track", Description: "Synthetic private proposal <literal>", Phases: []client.ProjectMapPhase{{ID: "foundation", Name: "Foundation", State: "Pending"}}, Links: []client.ProjectMapLink{}}
	provenance := client.MapImportProvenance{References: []string{"https://example.invalid/selected-source"}, Inferred: true, Confidence: "medium", Uncertainty: "Authored proposal from selected sources only"}
	other := entry
	other.Title = "Approved second track"
	document := client.MapImportDocument{Version: 1, Source: client.MapImportSource{Key: "packaged-map", RepositoryID: 991, Kind: "prepared", Snapshot: "approved-one", CollectedAt: "2026-10-17T00:00:00Z", TransformationVersion: 1, Scope: "Two selected tracks"}, Entries: []client.MapImportItem{{SourceKey: "track-one", Entry: entry, Provenance: provenance}, {SourceKey: "track-two", Entry: other, Provenance: provenance}}}
	writeFile := func(name string, value any) string {
		t.Helper()
		body, e := json.Marshal(value)
		if e != nil {
			t.Fatal("fixture encoding failed")
		}
		file := filepath.Join(private, name)
		if e = os.WriteFile(file, body, 0600); e != nil {
			t.Fatal("private approved file unavailable")
		}
		return file
	}
	originalFile := writeFile("approved-one.json", document)
	preview := func(file string) client.MapImportPreview {
		t.Helper()
		var out client.MapImportPreview
		decode(t, call("map", "import-preview", "--file", file), &out)
		return out
	}
	applyArgs := func(file, hash, key string) []string {
		return []string{"map", "import-apply", "--file", file, "--preview-sha256", hash, "--key", key}
	}
	readSource := func(revision string) client.MapImportRecord {
		t.Helper()
		var out client.MapImportRecord
		if revision == "" {
			decode(t, call("map", "import-read", "packaged-map"), &out)
		} else {
			decode(t, call("map", "import-revision", "packaged-map", revision), &out)
		}
		return out
	}
	jobsBefore, inboxBefore, publicationBefore := call("jobs", "assigned"), call("inbox", "list"), call("publication", "read")
	initial := preview(originalFile)
	if !initial.Ready || len(initial.Plan) != 2 || initial.Plan[0].Action != "create" || initial.Plan[1].Action != "create" {
		t.Fatal("initial packaged map plan")
	}
	originalArgs := applyArgs(originalFile, initial.PreviewSHA256, "packaged-map-original")
	originalReceipt := call(originalArgs...)
	if !bytes.Equal(originalReceipt, call(originalArgs...)) {
		t.Fatal("exact import retry changed receipt")
	}
	var receipt client.MapImportReceipt
	decode(t, originalReceipt, &receipt)
	if receipt.Revision != 1 || receipt.Created != 2 || receipt.Updated != 0 || receipt.Outcome != "imported" {
		t.Fatal("initial import receipt")
	}
	original := readSource("")
	if len(original.Mappings) != 2 || original.WorkerID != worker || original.Document.Source.RepositoryID != 991 || !original.Document.Entries[0].Provenance.Inferred || original.Document.Entries[0].Provenance.Confidence != "medium" {
		t.Fatal("initial source attribution missing")
	}
	firstID, secondID := original.Mappings[0].EntryID, original.Mappings[1].EntryID
	// Preserve a manual edit to the item omitted from the next source document.
	manualOther := other
	manualOther.Title = "Manual edit survives additive omission"
	manualOtherFile := writeFile("manual-other.json", manualOther)
	call("map", "replace", secondID, "--file", manualOtherFile, "--expect-revision", "1", "--key", "manual-omitted")
	document.Source.Snapshot = "approved-two"
	document.Entries = append([]client.MapImportItem{}, document.Entries[:1]...)
	document.Entries[0].ExpectedRevision = 1
	document.Entries[0].Entry.Title = "Approved updated first track"
	updateFile := writeFile("approved-two.json", document)
	updatePreview := preview(updateFile)
	if !updatePreview.Ready || len(updatePreview.Plan) != 1 || updatePreview.Plan[0].Action != "update" {
		t.Fatal("positive update not admitted")
	}
	updateArgs := applyArgs(updateFile, updatePreview.PreviewSHA256, "packaged-map-update")
	decode(t, call(updateArgs...), &receipt)
	if receipt.Revision != 2 || receipt.Updated != 1 || receipt.Created != 0 {
		t.Fatal("positive update receipt")
	}
	// This exercises the accepted independent finding: input still says expected
	// revision1, while its successfully imported target and mapping are now2.
	repeat := preview(updateFile)
	if !repeat.Ready || repeat.Plan[0].Action != "unchanged" || repeat.Plan[0].CurrentRevision != 2 {
		t.Fatal("retained positive update cannot be deliberately repeated")
	}
	decode(t, call(applyArgs(updateFile, repeat.PreviewSHA256, "packaged-map-update-new-key")...), &receipt)
	if receipt.Revision != 2 || receipt.Outcome != "unchanged" || receipt.Unchanged != 1 || receipt.Updated != 0 {
		t.Fatal("new key duplicated source revision")
	}
	latest := readSource("")
	var history struct {
		Items   []client.MapImportHistorySummary `json:"items"`
		HasMore bool                             `json:"has_more"`
	}
	decode(t, call("map", "import-history", "packaged-map", "--limit", "100"), &history)
	if len(history.Items) != 2 || history.HasMore || history.Items[0].Revision != 2 || history.Items[1].Revision != 1 || latest.Revision != 2 || len(latest.Mappings) != 1 || latest.Mappings[0].EntryID != firstID || latest.Mappings[0].EntryRevision != 2 {
		t.Fatal("source history/mappings lost or duplicated")
	}
	historical := readSource("1")
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(historical)
	if !bytes.Equal(a, b) {
		t.Fatal("original provenance was rewritten")
	}
	var retainedOther client.ProjectMapEntry
	decode(t, call("map", "read", secondID), &retainedOther)
	if retainedOther.Title != manualOther.Title || retainedOther.Revision != 2 {
		t.Fatal("additive omission overwrote manual target")
	}
	manual := document.Entries[0].Entry
	manual.Title = "Later manual edit must win"
	manualFile := writeFile("manual-current.json", manual)
	call("map", "replace", firstID, "--file", manualFile, "--expect-revision", "2", "--key", "manual-current")
	stale := preview(updateFile)
	if stale.Ready || stale.Conflicts != 1 || stale.PreviewSHA256 != "" {
		t.Fatal("stale retained update hid later manual edit")
	}
	// A safe refusal leaves source history unchanged. Exact original key remains
	// recoverable after all later source revisions and ordinary map edits.
	if !bytes.Equal(originalReceipt, call(originalArgs...)) {
		t.Fatal("historical exact retry no longer recoverable")
	}
	current := readSource("")
	if current.Revision != 2 {
		t.Fatal("reads/retry changed source history")
	}
	var manualCurrent client.ProjectMapEntry
	decode(t, call("map", "read", firstID), &manualCurrent)
	if manualCurrent.Title != manual.Title || manualCurrent.Revision != 3 {
		t.Fatal("original-key recovery overwrote current map")
	}
	if !jsonEqual(jobsBefore, call("jobs", "assigned")) || !jsonEqual(inboxBefore, call("inbox", "list")) || !jsonEqual(publicationBefore, call("publication", "read")) {
		t.Fatal("map import changed jobs, inbox or public publication")
	}
	if providers.Load() != 0 {
		t.Fatal("prepared import called an external provider")
	}
	t.Logf("packaged map import fc=%x fcd=%x; private refusal, preview/create/replay, positive-update new-key no-op, manual/omission provenance and history passed; HTTP=%d external=%d", fcHash, fcdHash, requests.Load(), providers.Load())
}
