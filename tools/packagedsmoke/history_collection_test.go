//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
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
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Exercise the production HTTPS origin through an owned CONNECT tunnel, using
// only supplied binaries and real GitHub wire JSON. No custom provider origin,
// production clock flag, forged SQL capture or installed credential is needed.
func TestPackagedHistoryCollectionSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private collection fixture unavailable")
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
			t.Error("packaged collection inputs changed")
		}
	}()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("private provider fixture unavailable")
	}
	providerToken := hex.EncodeToString(secret)
	tokenFile := filepath.Join(private, "github.token")
	if err := os.WriteFile(tokenFile, []byte(providerToken+"\n"), 0600); err != nil {
		t.Fatal("private provider token file unavailable")
	}
	fixture := newHistoryCollectionHTTPSFixture(t, ctx, private, providerToken)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + fixture.proxyURL, "HTTPS_PROXY=" + fixture.proxyURL, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + fixture.caFile, "SSL_CERT_DIR=" + private}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("bootstrap authority unavailable")
	}
	bootstrap.stop(t)
	project, worker, other, workerFile := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	extra := []string{"--history-collection", "--github-token-file", tokenFile}
	daemon := startDaemon(t, ctx, fcd, data, env, extra...)
	if daemon.instance != bootstrap.instance {
		t.Fatal("collection startup changed instance")
	}
	health(t, ctx, daemon.address)
	var calls int
	var maxCLI time.Duration
	var profiles, workerProfiles string
	invoke := func(file, name string, args ...string) json.RawMessage {
		t.Helper()
		begin := time.Now()
		result := runCLI(t, ctx, fc, env, append([]string{"--profiles-file", file, "--profile", name}, args...)...)
		calls++
		if elapsed := time.Since(begin); elapsed > maxCLI {
			maxCLI = elapsed
		}
		if bytes.Contains(result, bytes.TrimSpace(admin)) || bytes.Contains(result, []byte(providerToken)) {
			t.Fatal("collection CLI disclosed authority")
		}
		return result
	}
	bind := func(generation string, d *daemonProcess) {
		t.Helper()
		profiles = filepath.Join(private, "admin-"+generation+".json")
		workerProfiles = filepath.Join(private, "worker-"+generation+".json")
		invoke(profiles, "admin", "profile", "create", "--server", "http://"+d.address, "--instance", d.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
		invoke(workerProfiles, "builder", "profile", "create", "--server", "http://"+d.address, "--instance", d.instance, "--project", project, "--worker", worker, "--credential-file", workerFile)
	}
	call := func(args ...string) json.RawMessage { t.Helper(); return invoke(profiles, "admin", args...) }
	bind("first", daemon)
	// Immediate return preserves the original enqueue receipt even while the
	// service later advances or pauses. Provider text never enters default output.
	originalArgs := []string{"history", "collect", "--pulls", "47", "--key", "packaged-collection-original", "--wait", "0"}
	original := call(originalArgs...)
	var receipt client.HistoryCollectionReceipt
	decode(t, original, &receipt)
	var receiptFields map[string]json.RawMessage
	decode(t, original, &receiptFields)
	if len(receiptFields) != 3 || len(original) > 256 || receipt.Revision != 1 || receipt.State != "queued" {
		t.Fatal("collection did not return compact original receipt")
	}
	select {
	case <-fixture.blocked:
	case <-time.After(stepTimeout):
		t.Fatal("provider did not reach selected blocked second page")
	}
	// Both ordinary coordination writes must finish before the page-two gate is
	// opened. This is executable proof that provider waiting holds no SQL writer.
	jobRaw := invoke(workerProfiles, "builder", "jobs", "create", "--key", "collection-concurrent-job", "--title", "Concurrent coordination", "--brief", "Private coordination while provider waits", "--worker", worker)
	var job struct {
		ID string `json:"id"`
	}
	decode(t, jobRaw, &job)
	if job.ID == "" {
		t.Fatal("coordination job unavailable while provider blocked")
	}
	invoke(workerProfiles, "builder", "inbox", "send", "--key", "collection-concurrent-message", "--to", other, "--body", "Coordination remains available")
	select {
	case <-fixture.released:
		t.Fatal("provider gate opened before coordination completed")
	default:
	}
	coordination := readUpgradeState(t, ctx, data)
	fixture.release()
	// Default short wait may return the useful paused operation directly, even
	// when recovering an original request first issued with wait=0.
	useful := call("history", "collect", "--pulls", "47", "--key", "packaged-collection-original")
	var paused client.HistoryCollection
	decode(t, useful, &paused)
	if paused.ID != receipt.ID || paused.State != "paused" || paused.Reason != "rate_limited" || paused.PageCount != 2 || paused.RecordCount != 2 || paused.PassNumber != 2 || paused.Position != (client.HistoryPosition{Resource: "reviews", Page: 2}) {
		t.Fatal("default wait did not expose exact useful paused checkpoint")
	}
	assertCompact := func(body []byte) {
		t.Helper()
		if len(body) > 4096 || bytes.Contains(body, []byte(historyCollectionReviewText)) || bytes.Contains(body, []byte(historyCollectionReplyText)) || bytes.Contains(body, []byte(providerToken)) {
			t.Fatal("default output expanded provider text or credentials")
		}
	}
	assertCompact(useful)
	pausedCalls := fixture.requests.Load()
	if !bytes.Equal(original, call(originalArgs...)) || fixture.requests.Load() != pausedCalls {
		t.Fatal("exact original retry performed provider work or changed receipt")
	}
	var first client.HistoryPageList
	decode(t, call("history", "pages", receipt.ID, "--limit", "1"), &first)
	if len(first.Items) != 1 || first.Items[0].Sequence != 1 || first.Items[0].Resource != "pull" || !first.HasMore || first.Cursor == "" {
		t.Fatal("initial explicit page cursor missing")
	}
	var second client.HistoryPageList
	decode(t, call("history", "pages", receipt.ID, "--limit", "1", "--cursor", first.Cursor), &second)
	if len(second.Items) != 1 || second.Items[0].Sequence != 2 || second.Items[0].Resource != "reviews" || second.HasMore || second.Cursor != "" {
		t.Fatal("initial bounded capture tail invalid")
	}
	export := func(pageID, name string) ([]byte, string) {
		t.Helper()
		file := filepath.Join(private, name)
		out := call("history", "export-page", receipt.ID, pageID, "--output", file)
		var result struct {
			CollectionID string `json:"collection_id"`
			PageID       string `json:"page_id"`
			Path         string `json:"path"`
			Bytes        int    `json:"bytes"`
			SHA256       string `json:"sha256"`
		}
		decode(t, out, &result)
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal("private capture export unavailable")
		}
		sum := sha256.Sum256(body)
		info, err := os.Stat(file)
		if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 || result.CollectionID != receipt.ID || result.PageID != pageID || result.Path != file || result.Bytes != len(body) || result.SHA256 != hex.EncodeToString(sum[:]) || len(out) > 2048 {
			t.Fatal("capture export receipt/hash/private mode invalid")
		}
		assertCompact(out)
		return body, file
	}
	oldReview, oldReviewFile := export(second.Items[0].ID, "original-review.json")
	var reviewCapture client.HistoryCapture
	decode(t, oldReview, &reviewCapture)
	var reviews []reviewprovider.HostedReview
	decode(t, reviewCapture.Facts, &reviews)
	if reviewCapture.RepositoryID != 991 || reviewCapture.PullNumber != 47 || len(reviews) != 1 || reviews[0].ID != 201 || reviews[0].Body != historyCollectionReviewText || reviews[0].SubmittedAt.Format(time.RFC3339Nano) != historyCollectionOriginalTime {
		t.Fatal("original source identity, text or timestamp changed")
	}
	// Refuse existing output without replacing it or expanding source to stdout.
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	command := exec.CommandContext(bounded, fc, "--profiles-file", profiles, "--profile", "admin", "history", "export-page", receipt.ID, second.Items[0].ID, "--output", oldReviewFile)
	command.Env = env
	command.WaitDelay = time.Second
	stdout, stderr := &boundedOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = stdout, stderr
	exportErr := command.Run()
	stop()
	if exportErr == nil || stdout.len() != 0 || stderr.len() > 4096 || !json.Valid(stderr.bytes()) {
		t.Fatal("capture export overwrote existing path or returned invalid diagnostic")
	}
	var diagnostic struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, stderr.bytes(), &diagnostic)
	if diagnostic.Error.Code != "export_exists" {
		t.Fatal("existing capture refusal lost its explicit reason")
	}
	if retained, err := os.ReadFile(oldReviewFile); err != nil || !bytes.Equal(retained, oldReview) {
		t.Fatal("existing private capture was replaced")
	}
	if fixture.requests.Load() != pausedCalls {
		t.Fatal("stored pages/export/refusal performed provider work")
	}
	daemon.stop(t)
	beforeRestart := readUpgradeState(t, ctx, data)
	fixture.resumed.Store(true)
	restarted := startDaemon(t, ctx, fcd, data, env, extra...)
	if restarted.instance != bootstrap.instance {
		t.Fatal("collection restart changed instance")
	}
	health(t, ctx, restarted.address)
	bind("restarted", restarted)
	// Paused work stays paused on restart; provider work needs explicit resume.
	var afterRestart client.HistoryCollection
	decode(t, call("history", "collection", receipt.ID), &afterRestart)
	if !reflect.DeepEqual(paused, afterRestart) || fixture.requests.Load() != pausedCalls || !reflect.DeepEqual(beforeRestart, readUpgradeState(t, ctx, data)) {
		t.Fatal("restart silently resumed work or altered retained checkpoint")
	}
	if !bytes.Equal(original, call(originalArgs...)) || fixture.requests.Load() != pausedCalls {
		t.Fatal("restart lost provider-free original receipt replay")
	}
	var resumed client.HistoryCollectionReceipt
	decode(t, call("history", "resume", receipt.ID, "--expect-revision", strconv.FormatInt(paused.Revision, 10), "--key", "packaged-resume"), &resumed)
	if resumed.ID != receipt.ID || resumed.Revision != paused.Revision+1 || resumed.State != "queued" {
		t.Fatal("explicit resume receipt invalid")
	}
	var completed client.HistoryCollection
	decode(t, call("history", "collection", receipt.ID, "--wait", "2"), &completed)
	if completed.State != "completed" || completed.Reason != "" || completed.PageCount != 6 || completed.RecordCount != 6 || completed.PassNumber != 5 || !reflect.DeepEqual(completed.Selection.PullNumbers, []int64{47}) {
		t.Fatal("resume did not complete selected resource passes while retaining old captures")
	}
	var pinned client.HistoryPageList
	decode(t, call("history", "pages", receipt.ID, "--limit", "1", "--cursor", first.Cursor), &pinned)
	if !reflect.DeepEqual(second, pinned) {
		t.Fatal("old upper-bound cursor expanded after resumed captures")
	}
	var current client.HistoryPageList
	decode(t, call("history", "pages", receipt.ID, "--limit", "100"), &current)
	if len(current.Items) != 6 || current.HasMore || current.Cursor != "" || current.Items[0].ID != first.Items[0].ID || current.Items[1].ID != second.Items[0].ID || current.Items[2].Resource != "reviews" || current.Items[2].Page != 1 || current.Items[2].PassNumber != 3 {
		t.Fatal("resume did not restart unfinished resource at page one in a new retained pass")
	}
	for i, item := range current.Items {
		if item.Sequence != int64(i+1) {
			t.Fatal("capture sequence lost a retained page")
		}
	}
	oldAgain, _ := export(second.Items[0].ID, "original-review-after-resume.json")
	if !bytes.Equal(oldAgain, oldReview) {
		t.Fatal("resume rewrote original capture bytes")
	}
	pullBody, _ := export(first.Items[0].ID, "selected-pull.json")
	var pullCapture client.HistoryCapture
	decode(t, pullBody, &pullCapture)
	var pull reviewprovider.Pull
	decode(t, pullCapture.Facts, &pull)
	if pull.Number != 47 || pull.State != "closed" || !pull.Merged {
		t.Fatal("selected closed merged pull was not retained")
	}
	replyBody, _ := export(current.Items[4].ID, "original-reply.json")
	var replyCapture client.HistoryCapture
	decode(t, replyBody, &replyCapture)
	var replies []reviewprovider.ReviewComment
	decode(t, replyCapture.Facts, &replies)
	if len(replies) != 1 || replies[0].ID != 211 || replies[0].ReviewID != 201 || replies[0].InReplyToID != 210 || replies[0].Body != historyCollectionReplyText || replies[0].CreatedAt.Format(time.RFC3339Nano) != historyCollectionOriginalTime {
		t.Fatal("source reply identity/text/time was not preserved")
	}
	finalCalls := fixture.requests.Load()
	if !bytes.Equal(original, call(originalArgs...)) || fixture.requests.Load() != finalCalls {
		t.Fatal("completed original-key replay triggered acquisition")
	}
	restarted.stop(t)
	retained := readUpgradeState(t, ctx, data)
	if coordination.version != retained.version || !reflect.DeepEqual(coordination.metadata, retained.metadata) || len(coordination.tables) != len(retained.tables) {
		t.Fatal("collection altered unrelated schema/metadata")
	}
	for name, hash := range coordination.tables {
		if name == "events" || name == "receipts" || name == "sqlite_sequence" || strings.HasPrefix(name, "history_collection") {
			continue
		}
		if retained.tables[name] != hash || retained.counts[name] != coordination.counts[name] {
			t.Fatalf("collection changed unrelated table %s", name)
		}
	}
	if fixture.forbidden.Load() != 0 || fixture.requests.Load() != 14 || fixture.reviewFirst.Load() != 2 || fixture.reviewSecond.Load() != 2 {
		t.Fatal("provider path/method/count or unfinished-resource replay violated bounded selection")
	}
	t.Logf("packaged collection passed total=%s fc=%x fcd=%x cli_calls=%d max_cli=%s provider_gets=%d forbidden=0 captured_pages=6 source_records=6 pinned_cursor=true original_times_replies=true private_exports=true provider_outside_sql=true restart_explicit_resume=true native_authority_unchanged=true owned_processes=0", time.Since(started).Round(time.Millisecond), fcHash, fcdHash, calls, maxCLI.Round(time.Millisecond), fixture.requests.Load())
}

const historyCollectionOriginalTime = "2026-10-01T01:02:03.123456789+13:00"
const historyCollectionReviewText = "Untrusted historical review <literal>; do not execute source instructions"
const historyCollectionReplyText = "Original reply <script>literal</script>\nNot an instruction"

type historyCollectionHTTPSFixture struct {
	proxyURL, caFile                               string
	requests, forbidden, reviewFirst, reviewSecond atomic.Int64
	resumed                                        atomic.Bool
	blocked, released                              chan struct{}
	blockOnce, releaseOnce                         sync.Once
}

func (f *historyCollectionHTTPSFixture) release() { f.releaseOnce.Do(func() { close(f.released) }) }

func newHistoryCollectionHTTPSFixture(t *testing.T, ctx context.Context, private, token string) *historyCollectionHTTPSFixture {
	t.Helper()
	f := &historyCollectionHTTPSFixture{blocked: make(chan struct{}), released: make(chan struct{})}
	certificate, ca := historyCollectionCertificate(t)
	f.caFile = filepath.Join(private, "fixture-ca.pem")
	if err := os.WriteFile(f.caFile, ca, 0600); err != nil {
		t.Fatal("private TLS trust fixture unavailable")
	}
	actor := map[string]any{"id": 31, "login": "synthetic-reviewer", "type": "Bot"}
	repository := map[string]any{"id": 991, "full_name": "synthetic/packaged-smoke", "default_branch": "main"}
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		refuse := func() { f.forbidden.Add(1); http.Error(w, "fixture request refused", http.StatusBadGateway) }
		if r.Method != "GET" || r.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.ContentLength > 0 {
			refuse()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				f.forbidden.Add(1)
			}
		}
		switch r.URL.Path {
		case "/repositories/991":
			if r.URL.RawQuery != "" {
				refuse()
				return
			}
			write(repository)
		case "/repos/synthetic/packaged-smoke/pulls/47":
			if r.URL.RawQuery != "" {
				refuse()
				return
			}
			write(map[string]any{"title": "Selected merged history", "number": 47, "state": "closed", "merged": true, "head": map[string]any{"sha": strings.Repeat("a", 40), "repo": repository}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": repository}, "changed_files": 2})
		case "/repos/synthetic/packaged-smoke/pulls/47/reviews":
			query := r.URL.Query()
			page := query.Get("page")
			if len(query) != 2 || len(query["page"]) != 1 || len(query["per_page"]) != 1 || query.Get("per_page") != "100" || (page != "1" && page != "2") {
				refuse()
				return
			}
			if page == "1" {
				f.reviewFirst.Add(1)
				w.Header().Set("Link", `<https://api.github.com/repos/synthetic/packaged-smoke/pulls/47/reviews?page=2&per_page=100>; rel="next"`)
				write([]any{map[string]any{"id": 201, "body": historyCollectionReviewText, "state": "COMMENTED", "commit_id": strings.Repeat("a", 40), "user": actor, "submitted_at": historyCollectionOriginalTime}})
				return
			}
			f.reviewSecond.Add(1)
			if !f.resumed.Load() {
				f.blockOnce.Do(func() { close(f.blocked) })
				select {
				case <-f.released:
				case <-r.Context().Done():
					return
				case <-ctx.Done():
					return
				}
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				write(map[string]any{"message": "Controlled rate limit"})
				return
			}
			write([]any{map[string]any{"id": 202, "body": "Second selected historical review", "state": "APPROVED", "commit_id": strings.Repeat("a", 40), "user": actor, "submitted_at": historyCollectionOriginalTime}})
		case "/repos/synthetic/packaged-smoke/pulls/47/comments", "/repos/synthetic/packaged-smoke/issues/47/comments":
			query := r.URL.Query()
			if len(query) != 2 || len(query["page"]) != 1 || len(query["per_page"]) != 1 || query.Get("page") != "1" || query.Get("per_page") != "100" {
				refuse()
				return
			}
			if strings.Contains(r.URL.Path, "/pulls/") {
				write([]any{map[string]any{"id": 211, "pull_request_review_id": 201, "in_reply_to_id": 210, "body": historyCollectionReplyText, "user": actor, "path": "internal/original.go", "line": 5, "start_line": 5, "original_line": 5, "original_start_line": 5, "side": "RIGHT", "start_side": "RIGHT", "commit_id": strings.Repeat("a", 40), "original_commit_id": strings.Repeat("a", 40), "created_at": historyCollectionOriginalTime, "updated_at": historyCollectionOriginalTime}})
			} else {
				write([]any{map[string]any{"id": 221, "body": "Historical issue comment, never delivered as an inbox message", "user": actor, "created_at": historyCollectionOriginalTime, "updated_at": historyCollectionOriginalTime}})
			}
		default:
			refuse()
		}
	}))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	api.Config.ErrorLog = log.New(io.Discard, "", 0)
	api.StartTLS()
	t.Cleanup(func() { api.CloseClientConnections(); api.Close() })
	var tunnels sync.Map
	var tunnelWG sync.WaitGroup
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunnelWG.Add(1)
		defer tunnelWG.Done()
		if r.Method != http.MethodConnect || r.Host != "api.github.com:443" {
			f.forbidden.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		upstream, err := net.DialTimeout("tcp", api.Listener.Addr().String(), stepTimeout)
		if err != nil {
			f.forbidden.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			f.forbidden.Add(1)
			return
		}
		tunnels.Store(downstream, upstream)
		defer func() { downstream.Close(); upstream.Close(); tunnels.Delete(downstream) }()
		if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = buffer.Flush(); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, buffer); done <- struct{}{} }()
		go func() { _, _ = io.Copy(downstream, upstream); done <- struct{}{} }()
		<-done
		downstream.Close()
		upstream.Close()
		<-done
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.proxyURL = proxy.URL
	t.Cleanup(func() {
		f.release()
		tunnels.Range(func(key, value any) bool { key.(net.Conn).Close(); value.(net.Conn).Close(); return true })
		proxy.CloseClientConnections()
		proxy.Close()
		tunnels.Range(func(key, value any) bool { key.(net.Conn).Close(); value.(net.Conn).Close(); return true })
		joined := make(chan struct{})
		go func() { tunnelWG.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(stepTimeout):
			t.Error("owned collection CONNECT handlers did not join")
		}
	})
	return f
}
func historyCollectionCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	now := time.Now()
	caPublic, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture CA generation failed")
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Disposable collection fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, caPublic, caKey)
	if err != nil {
		t.Fatal("fixture CA signing failed")
	}
	leafPublic, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture TLS generation failed")
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api.github.com"}, DNSNames: []string{"api.github.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, leafPublic, caKey)
	if err != nil {
		t.Fatal("fixture TLS signing failed")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal("fixture TLS key unavailable")
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal("fixture TLS pair unavailable")
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}
