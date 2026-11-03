package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const historyCLISource = "PRIVATE_SELECTED_HISTORY_SOURCE"

type historyCLIProvider struct {
	calls      atomic.Int64
	failSecond atomic.Bool
	entered    chan struct{}
	release    chan struct{}
}

func (p *historyCLIProvider) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	p.calls.Add(1)
	return reviewprovider.Repository{ID: id, FullName: "renamed/project", DefaultBranch: "main"}, ctx.Err()
}
func (p *historyCLIProvider) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	p.calls.Add(1)
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return reviewprovider.Pull{}, ctx.Err()
		}
	}
	return reviewprovider.Pull{Title: historyCLISource, Number: n, RepositoryID: 1, State: "closed", Merged: n == 47, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, ctx.Err()
}
func (p *historyCLIProvider) ReviewsPage(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
	p.calls.Add(1)
	if page == 2 && p.failSecond.Load() {
		return reviewprovider.Page[reviewprovider.HostedReview]{}, &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryAfterSeconds: 12, Message: "controlled rate hold"}
	}
	if page > 1 {
		return reviewprovider.Page[reviewprovider.HostedReview]{Items: []reviewprovider.HostedReview{}}, ctx.Err()
	}
	next := int64(0)
	if n == 47 {
		next = 2
	}
	at, _ := time.Parse(time.RFC3339Nano, "2026-10-01T01:02:03.123456789+13:00")
	return reviewprovider.Page[reviewprovider.HostedReview]{Items: []reviewprovider.HostedReview{{ID: 201, Body: historyCLISource, State: "commented", HeadSHA: strings.Repeat("a", 40), Author: reviewprovider.Author{ID: 7, Login: "provider", Kind: "Bot"}, SubmittedAt: at}}, NextPage: next}, ctx.Err()
}
func (p *historyCLIProvider) ReviewCommentsPage(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.ReviewComment], error) {
	p.calls.Add(1)
	return reviewprovider.Page[reviewprovider.ReviewComment]{Items: []reviewprovider.ReviewComment{{ID: 301, ReviewID: 201, InReplyToID: 300, Body: historyCLISource, Author: reviewprovider.Author{ID: 7, Login: "provider", Kind: "Bot"}, Path: "source.go", HeadSHA: strings.Repeat("a", 40), OriginalHeadSHA: strings.Repeat("b", 40)}}}, ctx.Err()
}
func (p *historyCLIProvider) IssueCommentsPage(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.IssueComment], error) {
	p.calls.Add(1)
	return reviewprovider.Page[reviewprovider.IssueComment]{Items: []reviewprovider.IssueComment{{ID: 401, Body: historyCLISource, Author: reviewprovider.Author{ID: 8, Login: "source", Kind: "User"}}}}, ctx.Err()
}

type historyCLIService struct {
	f                         *serviceFixture
	e                         *historycollect.Engine
	p                         *historyCLIProvider
	project, worker, profiles string
	requests                  atomic.Int64
	pageReads                 atomic.Int64
	posts                     atomic.Int64
	stop                      context.CancelFunc
	done                      <-chan error
}

func historyCLISetup(t *testing.T, p *historyCLIProvider, start bool) *historyCLIService {
	t.Helper()
	f := setupServiceFixture(t)
	project := f.project(t, "selected-history")
	worker := f.worker(t, project, "Collector")
	if err := historycollect.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	e, err := historycollect.New(f.store, p, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	h := &historyCLIService{f: f, e: e, p: p, project: project, worker: worker}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	e.Register(mux)
	f.handler = mux
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pages") {
			h.pageReads.Add(1)
		}
		if r.Method == "POST" {
			h.posts.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	h.profiles = mutationProfiles(t, f, service.URL, map[string][2]string{"admin": {project, worker}})
	t.Cleanup(func() {
		if h.stop != nil {
			h.stop()
			select {
			case err := <-h.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("owned collection worker did not stop")
			}
		}
		service.Close()
	})
	if start {
		h.start(t)
	}
	return h
}
func (h *historyCLIService) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := h.e.Run(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.stop, h.done = cancel, done
}
func (h *historyCLIService) invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return mutationInvoke(t, h.profiles, "admin", append([]string{"history"}, args...)...)
}
func historyCLIJSON[T any](t *testing.T, code int, out, stderr string) T {
	t.Helper()
	var v T
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &v) != nil {
		t.Fatalf("history command code=%d output_bytes=%d diagnostic=%s", code, len(out), stderr)
	}
	if strings.Contains(out, historyCLISource) {
		t.Fatal("default output expanded private provider source")
	}
	return v
}
func (h *historyCLIService) scope(t *testing.T) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.SetPathValue("project_id", h.project)
	r.Header.Set("Authorization", "Bearer "+h.f.admin)
	r.Header.Set("X-FC-Worker", h.worker)
	r.Header.Set("X-FC-Instance", h.f.store.InstanceID())
	s, err := h.f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHistoryCollectionCLIRealCollectionCompactPagesAndPrivateExport(t *testing.T) {
	h := historyCLISetup(t, &historyCLIProvider{}, true)
	code, out, stderr := h.invoke(t, "collect", "--pulls", "47,12", "--key", "selected-history")
	o := historyCLIJSON[historycollect.Operation](t, code, out, stderr)
	if o.State != "completed" || o.PageCount != 9 || o.RecordCount != 8 || !reflect.DeepEqual(o.Selection.PullNumbers, []int64{47, 12}) || len(out) > 2048 || h.p.calls.Load() != 18 {
		t.Fatal("selected closed/merged collection did not complete compactly")
	}
	calls := h.p.calls.Load()
	code, out, stderr = h.invoke(t, "collect", "--pulls", "47,12", "--key", "selected-history", "--wait", "0")
	receipt := historyCLIJSON[historycollect.Receipt](t, code, out, stderr)
	if receipt.ID != o.ID || receipt.Revision != 1 || receipt.State != "queued" || h.p.calls.Load() != calls {
		t.Fatal("exact key replay reenqueued or changed original receipt")
	}
	before := h.requests.Load()
	pagesBefore := h.pageReads.Load()
	code, out, stderr = h.invoke(t, "pages", o.ID)
	page := historyCLIJSON[historycollect.PageList](t, code, out, stderr)
	if len(page.Items) != 5 || !page.HasMore || page.Cursor == "" || h.requests.Load() != before+2 || h.pageReads.Load() != pagesBefore+1 || h.p.calls.Load() != calls {
		t.Fatal("default page read drained archive or contacted provider")
	}
	code, out, stderr = h.invoke(t, "pages", o.ID, "--cursor", page.Cursor)
	tail := historyCLIJSON[historycollect.PageList](t, code, out, stderr)
	if len(tail.Items) != 4 || tail.HasMore || tail.Items[0].Sequence != 6 {
		t.Fatal("explicit archive continuation changed")
	}
	// Export both object-valued PR facts and array-valued review facts; neither
	// source body may reach stdout, and the selected path is never overwritten.
	for _, item := range page.Items[:2] {
		output := filepath.Join(t.TempDir(), "selected-capture.json")
		code, out, stderr = h.invoke(t, "export-page", o.ID, item.ID, "--output", output)
		exported := historyCLIJSON[struct {
			CollectionID string `json:"collection_id"`
			PageID       string `json:"page_id"`
			Path         string `json:"path"`
			Bytes        int    `json:"bytes"`
			SHA256       string `json:"sha256"`
		}](t, code, out, stderr)
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		var capture historycollect.Capture
		if json.Unmarshal(data, &capture) != nil || capture.Resource != item.Resource || !bytes.Contains(data, []byte(historyCLISource)) || exported.CollectionID != o.ID || exported.PageID != item.ID || exported.Path != output || exported.Bytes != len(data) || exported.SHA256 != hex.EncodeToString(hash[:]) || len(out) > 1024 {
			t.Fatal("private export bytes/identity/hash changed")
		}
		info, err := os.Stat(output)
		if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			t.Fatal("capture export is not owner-private")
		}
		code, out, stderr = h.invoke(t, "export-page", o.ID, item.ID, "--output", output)
		after, _ := os.ReadFile(output)
		if code == 0 || out != "" || !strings.Contains(stderr, "export_exists") || !bytes.Equal(data, after) {
			t.Fatal("existing export replaced")
		}
		link := filepath.Join(t.TempDir(), "link.json")
		if err := os.Symlink(output, link); err == nil {
			code, out, stderr = h.invoke(t, "export-page", o.ID, item.ID, "--output", link)
			after, _ = os.ReadFile(output)
			if code == 0 || out != "" || !strings.Contains(stderr, "export_exists") || !bytes.Equal(data, after) {
				t.Fatal("export followed/replaced symlink")
			}
		}
	}
	if h.p.calls.Load() != calls {
		t.Fatal("retained export performed provider work")
	}
}

func TestHistoryCollectionCLIResumeCASReplayAndPinnedArchive(t *testing.T) {
	p := &historyCLIProvider{}
	p.failSecond.Store(true)
	h := historyCLISetup(t, p, true)
	code, out, stderr := h.invoke(t, "collect", "--pulls", "47", "--key", "paused-history")
	o := historyCLIJSON[historycollect.Operation](t, code, out, stderr)
	if o.State != "paused" || o.Reason != "rate_limited" || o.PageCount != 2 {
		t.Fatal("controlled rate pause missing")
	}
	code, out, stderr = h.invoke(t, "pages", o.ID, "--limit", "1")
	first := historyCLIJSON[historycollect.PageList](t, code, out, stderr)
	if len(first.Items) != 1 || !first.HasMore {
		t.Fatal("fixture missing pinned continuation")
	}
	calls := p.calls.Load()
	code, out, stderr = h.invoke(t, "resume", o.ID, "--expect-revision", strconv.FormatInt(o.Revision-1, 10), "--key", "stale-resume")
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "revision_conflict") || p.calls.Load() != calls {
		t.Fatal("stale resume mutated or fetched provider")
	}
	p.failSecond.Store(false)
	args := []string{"resume", o.ID, "--expect-revision", strconv.FormatInt(o.Revision, 10), "--key", "resume-history"}
	code, out, stderr = h.invoke(t, args...)
	receipt := historyCLIJSON[historycollect.Receipt](t, code, out, stderr)
	code, out, stderr = h.invoke(t, "collection", o.ID, "--wait", "2")
	completed := historyCLIJSON[historycollect.Operation](t, code, out, stderr)
	if completed.State != "completed" || completed.PageCount != 6 {
		t.Fatal("explicit resume did not append complete new pass")
	}
	code, out, stderr = h.invoke(t, args...)
	replay := historyCLIJSON[historycollect.Receipt](t, code, out, stderr)
	if replay != receipt {
		t.Fatal("resume replay changed original receipt")
	}
	before := h.requests.Load()
	code, out, stderr = h.invoke(t, "pages", o.ID, "--cursor", first.Cursor, "--limit", "100")
	pinned := historyCLIJSON[historycollect.PageList](t, code, out, stderr)
	if len(pinned.Items) != 1 || pinned.HasMore || pinned.Items[0].Sequence != 2 || h.requests.Load() != before+2 {
		t.Fatal("pinned archive expanded into later resumed captures")
	}
}

type historyCLIShortWriter struct{}

func (historyCLIShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestHistoryCollectionCLIShortOutputRecoveryAndGuardedCancel(t *testing.T) {
	h := historyCLISetup(t, &historyCLIProvider{}, false)
	args := []string{"--profiles-file", h.profiles, "--profile", "admin", "history", "collect", "--pulls", "47", "--key", "lost-output", "--wait", "0"}
	var stderr bytes.Buffer
	code := cli.RunContext(t.Context(), args, nil, historyCLIShortWriter{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || !strings.Contains(stderr.String(), "output_failed") {
		t.Fatal("short stdout did not preserve mutation uncertainty")
	}
	code, out, diagnostic := h.invoke(t, "collect", "--pulls", "47", "--key", "lost-output", "--wait", "0")
	r := historyCLIJSON[historycollect.Receipt](t, code, out, diagnostic)
	var count int
	h.f.store.DB().QueryRow(`SELECT count(*) FROM history_collections`).Scan(&count)
	if count != 1 || h.p.calls.Load() != 0 {
		t.Fatal("lost-output recovery duplicated or dispatched collection")
	}
	code, out, diagnostic = h.invoke(t, "cancel", r.ID, "--expect-revision", "2", "--key", "stale-cancel")
	if code != cli.ExitConflict || out != "" || !strings.Contains(diagnostic, "revision_conflict") {
		t.Fatal("stale cancel accepted")
	}
	cancelArgs := []string{"cancel", r.ID, "--expect-revision", "1", "--key", "cancel-once"}
	code, out, diagnostic = h.invoke(t, cancelArgs...)
	canceled := historyCLIJSON[historycollect.Receipt](t, code, out, diagnostic)
	code, out, diagnostic = h.invoke(t, cancelArgs...)
	again := historyCLIJSON[historycollect.Receipt](t, code, out, diagnostic)
	if canceled != again || canceled.State != "canceled" {
		t.Fatal("cancel replay lost exact receipt")
	}
}

func TestHistoryCollectionCLICanceledWaitLeavesAcquisitionActive(t *testing.T) {
	p := &historyCLIProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h := historyCLISetup(t, p, true)
	code, out, stderr := h.invoke(t, "collect", "--pulls", "12", "--key", "wait-detaches", "--wait", "0")
	r := historyCLIJSON[historycollect.Receipt](t, code, out, stderr)
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not block")
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan int, 1)
	var stdout, diagnostic bytes.Buffer
	before := h.requests.Load()
	posts := h.posts.Load()
	go func() {
		result <- cli.RunContext(ctx, []string{"--profiles-file", h.profiles, "--profile", "admin", "history", "collection", r.ID, "--wait", "30"}, nil, &stdout, &diagnostic)
	}()
	deadline := time.After(time.Second)
	for h.requests.Load() < before+2 {
		select {
		case <-deadline:
			cancel()
			close(p.release)
			t.Fatal("long wait did not reach retained GET")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case code := <-result:
		if code != cli.ExitCancelled || stdout.Len() != 0 {
			t.Fatal("wait cancellation did not detach")
		}
	case <-time.After(time.Second):
		close(p.release)
		t.Fatal("CLI wait did not cancel")
	}
	op, err := h.e.Read(t.Context(), h.scope(t), r.ID)
	if err != nil || op.State != "running" || h.posts.Load() != posts {
		close(p.release)
		t.Fatal("canceled wait canceled action", err)
	}
	close(p.release)
	code, out, stderr = h.invoke(t, "collection", r.ID, "--wait", "2")
	done := historyCLIJSON[historycollect.Operation](t, code, out, stderr)
	if done.State != "completed" {
		t.Fatal("detached action did not finish")
	}
}

func TestHistoryCollectionCLIInvalidArgumentsBeforeCredentials(t *testing.T) {
	id := "history_collection_" + strings.Repeat("a", 32)
	page := "history_page_" + strings.Repeat("b", 32)
	for _, args := range [][]string{
		{"collect"}, {"collect", "--pulls", "47"}, {"collect", "--pulls", "47,47", "--key", "key"}, {"collect", "--pulls", "047", "--key", "key"}, {"collect", "--pulls", "0", "--key", "key"}, {"collect", "--pulls", "2147483648", "--key", "key"}, {"collect", "--pulls", "47,", "--key", "key"}, {"collect", "--pulls", "47", "--key", "bad\nkey"}, {"collect", "--pulls", "47", "--key", "key", "--wait", "901"}, {"collect", "--pulls", "47", "--key", "key", "--wait", "-1"}, {"collect", "--pulls", "47", "--key", "key", "--wait", "01"}, {"collect", "--pulls", "47", "--key", "key", "--wait", "0", "--wait", "0"},
		{"collection", "bad"}, {"collection", id, "--key", "key"}, {"pages", id, "--limit", "0"}, {"pages", id, "--limit", "101"}, {"pages", id, "--cursor", "bad cursor"}, {"resume", id, "--key", "key"}, {"cancel", id, "--key", "key", "--expect-revision", "0"}, {"export-page", id, page, "--output", "relative.json"}, {"export-page", id, "bad", "--output", filepath.Join(t.TempDir(), "x")},
	} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing-private-profile", "history"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") || strings.Contains(stderr, "profiles_") {
			t.Fatalf("invalid history grammar reached credentials: %v code=%d", args, code)
		}
	}
	numbers := make([]string, 101)
	for i := range numbers {
		numbers[i] = fmt.Sprint(i + 1)
	}
	code, out, stderr := run([]string{"--profiles-file", "/missing-private-profile", "history", "collect", "--pulls", strings.Join(numbers, ","), "--key", "key"}, nil)
	if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
		t.Fatal("unbounded selection reached credentials")
	}
}

func TestHistoryCollectionCLIExportPreflightBeforeCredentialsOrNetwork(t *testing.T) {
	id := "history_collection_" + strings.Repeat("a", 32)
	page := "history_page_" + strings.Repeat("b", 32)
	file := filepath.Join(t.TempDir(), "existing.json")
	original := []byte("private existing file")
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	paths := []struct{ path, code string }{{file, "export_exists"}, {t.TempDir(), "export_exists"}, {filepath.Join(t.TempDir(), "missing", "capture.json"), "export_failed"}}
	link := filepath.Join(t.TempDir(), "link")
	if os.Symlink(file, link) == nil {
		paths = append(paths, struct{ path, code string }{link, "export_exists"})
	}
	for _, tc := range paths {
		code, out, stderr := run([]string{"--profiles-file", "/missing-private-profile", "history", "export-page", id, page, "--output", tc.path}, nil)
		if code == 0 || out != "" || !strings.Contains(stderr, tc.code) || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, string(original)) {
			t.Fatal("unsafe export target reached credentials or disclosed contents")
		}
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("preflight altered existing file")
	}
}
