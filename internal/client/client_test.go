package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
)

const testToken = "synthetic-private-bearer-credential-12345"

func TestSharedArgumentValidatorBoundaries(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"job-one", true}, {"job:one", true}, {"é", true}, {strings.Repeat("é", 64), true}, {strings.Repeat("a", 128), true}, {"a.b", true},
		{"", false}, {".", false}, {"..", false}, {strings.Repeat("a", 129), false}, {strings.Repeat("é", 65), false}, {" padded", false}, {"padded ", false}, {"a b", false}, {"a\u00a0b", false}, {"a\u0085b", false}, {"a\nb", false}, {"a\x00b", false}, {"a/b", false}, {"a\\b", false}, {"a%20b", false}, {"a?b", false}, {"a#b", false}, {string([]byte{255}), false},
	} {
		if got := client.ValidID(test.value); got != test.valid {
			t.Errorf("resource ID %q: got %v, want %v", test.value, got, test.valid)
		}
	}
	for _, status := range []string{"active", "parked", "blocked", "completed"} {
		if !client.ValidStatus(status) {
			t.Errorf("valid status refused: %q", status)
		}
	}
	for _, status := range []string{"", "open", "Active", " active", "active ", "unknown", "completed\x00"} {
		if client.ValidStatus(status) {
			t.Errorf("invalid status accepted: %q", status)
		}
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"key", true}, {"a b", true}, {"key:operation-1", true}, {strings.Repeat("k", 128), true}, {"!~", true},
		{"", false}, {" key", false}, {"key ", false}, {strings.Repeat("k", 129), false}, {"key\nnext", false}, {"key\rnext", false}, {"key\tnext", false}, {"key\x00next", false}, {"key\x7f", false}, {"é", false}, {string([]byte{255}), false},
	} {
		if got := client.ValidMutationKey(test.value); got != test.valid {
			t.Errorf("mutation key %q: got %v, want %v", test.value, got, test.valid)
		}
	}
}

func binding(t *testing.T, base string) profile.Binding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-credential.token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return profile.Binding{Context: profile.Context{Profile: "chosen", ServerURL: base, InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: path}
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func handshake(w http.ResponseWriter) {
	write(w, 200, map[string]string{"instance_id": "instance-one"})
}
func checkError(t *testing.T, err error, code string, paths ...string) {
	t.Helper()
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != code || failure.Message == "" || failure.Action == "" {
		t.Fatalf("unexpected safe diagnostic: %v", err)
	}
	data, e := json.Marshal(failure)
	if e != nil {
		t.Fatal(e)
	}
	for _, private := range append(paths, testToken) {
		if private != "" && (strings.Contains(string(data), private) || strings.Contains(failure.Error(), private)) {
			t.Fatal("diagnostic exposed credential information")
		}
	}
}
func newClient(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	c, err := client.New(context.Background(), binding(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestBoundReadsPreservePrefixHeadersQueriesAndTypedFields(t *testing.T) {
	var calls atomic.Int64
	job := client.Job{ID: "job:one", ProjectID: "project-one", Title: "title", Brief: "explicit full brief", WorkerID: "worker-one", Status: "active", Revision: 2, CreatedAt: "2026-10-15T00:00:00Z", UpdatedAt: "2026-10-15T00:01:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" {
			t.Error("unbound or mutating request")
		}
		if r.URL.Path == "/configured/prefix/api/v1/instance" {
			if r.Header.Get("X-FC-Worker") != "" {
				t.Error("handshake supplied worker")
			}
			handshake(w)
			return
		}
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("scoped worker missing")
		}
		prefix := "/configured/prefix/api/v1/projects/project-one"
		switch r.URL.Path {
		case prefix + "/workers/worker-one":
			write(w, 200, client.Worker{ID: "worker-one", ProjectID: "project-one", Name: "Builder"})
		case prefix + "/workers":
			if r.URL.RawQuery != "" {
				t.Error("zero options should omit query")
			}
			write(w, 200, client.Page[client.Worker]{Items: []client.Worker{{ID: "worker-one", ProjectID: "project-one", Name: "Builder"}}})
		case prefix + "/jobs":
			if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "cursor&one=two" || r.URL.Query().Get("worker_id") != "worker-one" || r.URL.Query().Get("status") != "active" {
				t.Error("job query changed")
			}
			write(w, 200, client.Page[client.JobSummary]{Items: []client.JobSummary{{ID: job.ID, Title: job.Title, WorkerID: job.WorkerID, Status: job.Status, Revision: job.Revision}}, Cursor: "next-cursor", HasMore: true})
		case prefix + "/jobs/job:one":
			if !strings.Contains(r.RequestURI, "job:one") {
				t.Error("escaped segment lost identity")
			}
			write(w, 200, job)
		case prefix + "/jobs/job:one/history":
			write(w, 200, client.Page[client.JobHistoryEntry]{Items: []client.JobHistoryEntry{{Revision: 2, ActorID: "principal-one", State: job, CreatedAt: job.UpdatedAt}}})
		case prefix + "/inbox/worker-one":
			if r.URL.Query().Get("unread") != "true" || r.URL.Query().Get("unacknowledged") != "true" {
				t.Error("inbox filters changed")
			}
			write(w, 200, client.Page[client.InboxItem]{Items: []client.InboxItem{{ID: "message-one", ConversationID: "conversation-one", SenderWorkerID: "sender-one", RecipientWorkerID: "worker-one", Preview: "compact preview", PreviewTruncated: true, CreatedAt: job.CreatedAt}}})
		case prefix + "/conversations/conversation-one":
			write(w, 200, client.ConversationPage{ConversationID: "conversation-one", RootMessageID: "message-one", Items: []client.Message{{ID: "message-one", ConversationID: "conversation-one", SenderWorkerID: "sender-one", RecipientWorkerID: "worker-one", Body: "  full immutable body\n", CreatedAt: job.CreatedAt}}})
		default:
			t.Errorf("prefix/path escaped binding: %s", r.URL.Path)
			write(w, 404, map[string]string{})
		}
	}))
	defer server.Close()
	b := binding(t, server.URL+"/configured/prefix")
	c, err := client.New(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Identity() != b.Context {
		t.Fatal("verified identity changed")
	}
	if value, err := c.Worker(context.Background()); err != nil || value.Name != "Builder" {
		t.Fatalf("worker: %v", err)
	}
	if value, err := c.Workers(context.Background(), client.PageOptions{}); err != nil || len(value.Items) != 1 {
		t.Fatalf("workers: %v", err)
	}
	if value, err := c.Jobs(context.Background(), client.PageOptions{Limit: 1, Cursor: "cursor&one=two"}, client.JobFilter{WorkerID: "worker-one", Status: "active"}); err != nil || value.Items[0].Revision != 2 || !value.HasMore || value.Cursor != "next-cursor" {
		t.Fatalf("jobs: %v", err)
	}
	if value, err := c.Job(context.Background(), job.ID); err != nil || value != job {
		t.Fatalf("job detail: %v", err)
	}
	if value, err := c.JobHistory(context.Background(), job.ID, client.PageOptions{}); err != nil || value.Items[0].State != job || value.Items[0].ActorID != "principal-one" {
		t.Fatalf("history: %v", err)
	}
	if value, err := c.Inbox(context.Background(), client.PageOptions{}, client.InboxFilter{Unread: true, Unacknowledged: true}); err != nil || !value.Items[0].PreviewTruncated || value.Items[0].Seen || value.Items[0].Acknowledged {
		t.Fatalf("inbox: %v", err)
	}
	if value, err := c.Conversation(context.Background(), "conversation-one", client.PageOptions{}); err != nil || value.Items[0].Body != "  full immutable body\n" || value.RootMessageID != "message-one" {
		t.Fatalf("conversation: %v", err)
	}
	if calls.Load() != 8 {
		t.Fatal("unexpected retry or paging request")
	}
	encoded, _ := json.Marshal(c)
	if string(encoded) != "{}" {
		t.Fatal("client serialized private state")
	}
}

func TestIdentityMismatchAndRedirectDoNotExposeCredentials(t *testing.T) {
	var forwarded atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	for _, redirect := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, destination.URL, http.StatusFound)
				return
			}
			write(w, 200, map[string]string{"instance_id": "other-instance"})
		}))
		b := binding(t, server.URL)
		c, err := client.New(context.Background(), b)
		server.Close()
		if c != nil {
			c.Close()
			t.Fatal("unverified client constructed")
		}
		code := "instance_mismatch"
		if redirect {
			code = "redirect_rejected"
		}
		checkError(t, err, code, b.CredentialFile)
	}
	if forwarded.Load() != 0 {
		t.Fatal("redirect forwarded credentials")
	}
}

func TestInvalidBindingsAndReferencesFailBeforeNetwork(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handshake(w) }))
	defer server.Close()
	for _, bad := range []string{"../escape", ".", "..", "other/project", `other\project`, "%2fescape", "worker?query", "worker#fragment", " padded", "two words", ""} {
		b := binding(t, server.URL)
		b.Context.ProjectID = bad
		_, err := client.New(context.Background(), b)
		checkError(t, err, "binding_invalid", b.CredentialFile)
	}
	for _, bad := range []string{"http://remote.example", "https://user:password@remote.example", "https://remote.example?query", "https://remote.example#fragment", "ftp://remote.example"} {
		b := binding(t, bad)
		_, err := client.New(context.Background(), b)
		checkError(t, err, "binding_invalid", b.CredentialFile)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid binding made request")
	}
	c := newClient(t, server)
	before := calls.Load()
	for _, bad := range []string{"../escape", "..", "a/b", `a\b`, "%2e%2e", "a?b", "a#b", ""} {
		_, err := c.Job(context.Background(), bad)
		checkError(t, err, "validation_failed")
		_, err = c.Conversation(context.Background(), bad, client.PageOptions{})
		checkError(t, err, "validation_failed")
	}
	for _, options := range []client.PageOptions{{Limit: -1}, {Limit: 101}, {Cursor: strings.Repeat("a", 4097)}} {
		_, err := c.Workers(context.Background(), options)
		checkError(t, err, "validation_failed")
	}
	_, err := c.Jobs(context.Background(), client.PageOptions{}, client.JobFilter{WorkerID: "../worker"})
	checkError(t, err, "validation_failed")
	_, err = c.Jobs(context.Background(), client.PageOptions{}, client.JobFilter{Status: "unknown"})
	checkError(t, err, "validation_failed")
	if calls.Load() != before {
		t.Fatal("invalid read made request")
	}
}

func TestStrictAndBoundedResponsesCannotRepairOrBroadenResults(t *testing.T) {
	valid := `{"id":"worker-one","project_id":"project-one","name":"Builder"}`
	for _, body := range []string{`{}`, `null`, valid + `{}`, `{"id":"worker-one","id":"other","project_id":"project-one","name":"Builder"}`, `{"ID":"worker-one","project_id":"project-one","name":"Builder"}`, `{"id":"worker-one","project_id":"other-project","name":"Builder"}`, `{"id":"worker-one","project_id":"project-one","name":"bad-\ud800"}`, strings.Replace(valid, "Builder", string([]byte{255}), 1), strings.Replace(valid, "Builder", testToken, 1), strings.Repeat(" ", maxBodyForTest+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, server)
		_, err := c.Worker(context.Background())
		checkError(t, err, "invalid_response")
		c.Close()
		server.Close()
	}
	for _, body := range []string{`{"items":null,"has_more":false}`, `{"items":[],"has_more":true,"cursor":"next"}`, `{"items":[],"has_more":false,"cursor":"unexpected"}`, `{"items":[],"has_more":true}`, `{"items":[],"has_more":false,"cursor":null}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, server)
		_, err := c.Workers(context.Background(), client.PageOptions{})
		checkError(t, err, "invalid_response")
		c.Close()
		server.Close()
	}
}

const maxBodyForTest = 4 << 20

func TestServiceErrorsStayBoundedSafeAndStructured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 409, map[string]any{"error": map[string]any{"code": "revision_conflict", "message": "safe message", "details": map[string]any{"current_revision": 7, "unexpected": "private provider text"}}, "request_id": "request-one"})
	}))
	c := newClient(t, server)
	_, err := c.Worker(context.Background())
	checkError(t, err, "revision_conflict")
	var failure *client.Error
	_ = errors.As(err, &failure)
	if failure.Status != 409 || failure.RequestID != "request-one" || failure.Details.(map[string]any)["current_revision"] != int64(7) || len(failure.Details.(map[string]any)) != 1 {
		t.Fatal("safe structured conflict lost or broadened")
	}
	c.Close()
	server.Close()
	for _, kind := range []string{"html", "message", "request_id", "code", "details", "reflected-token", "reflected-path", "duplicate"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			value := map[string]any{"error": map[string]any{"code": "forbidden", "message": "refused"}, "request_id": "request-one"}
			inner := value["error"].(map[string]any)
			switch kind {
			case "html":
				http.Error(w, "<h1>PRIVATE_ERROR</h1>", 403)
				return
			case "message":
				inner["message"] = strings.Repeat("x", 513)
			case "request_id":
				value["request_id"] = strings.Repeat("x", 129)
			case "code":
				inner["code"] = strings.Repeat("a", 129)
			case "details":
				inner["details"] = map[string]string{"large": strings.Repeat("x", 4097)}
			case "reflected-token":
				inner["message"] = testToken
			case "reflected-path":
				inner["details"] = map[string]string{"path": r.Header.Get("X-Test-Path")}
			case "duplicate":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","code":"unauthorized","message":"refused"},"request_id":"request-one"}`))
				return
			}
			write(w, 403, value)
		}))
		b := binding(t, server.URL)
		if kind == "reflected-path" {
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 403, map[string]any{"error": map[string]any{"code": "forbidden", "message": b.CredentialFile}, "request_id": "request-one"})
			})
		}
		c, err := client.New(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Worker(context.Background())
		checkError(t, err, "invalid_response", b.CredentialFile)
		c.Close()
		server.Close()
	}
}

func TestDeadlinesAndCancellationBoundWaitingWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	c := newClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Worker(ctx)
	checkError(t, err, "deadline_exceeded")
	if time.Since(start) > time.Second {
		t.Fatal("caller deadline was extended")
	}
	before := calls.Load()
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = c.Worker(ctx)
	checkError(t, err, "canceled")
	if calls.Load() != before || before != 2 {
		t.Fatal("canceled read or timed-out read retried")
	}
}

func TestCredentialKindsPermissionsSizeAndContentFailBeforeRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handshake(w) }))
	defer server.Close()
	for _, kind := range []string{"missing", "directory", "oversize", "long-token", "empty", "embedded-space", "embedded-newline", "control", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			b := binding(t, server.URL)
			switch kind {
			case "missing":
				b.CredentialFile = filepath.Join(t.TempDir(), "missing.token")
			case "directory":
				b.CredentialFile = t.TempDir()
			case "oversize":
				_ = os.WriteFile(b.CredentialFile, []byte(strings.Repeat("x", 4097)), 0600)
			case "long-token":
				_ = os.WriteFile(b.CredentialFile, []byte(strings.Repeat("x", 257)), 0600)
			case "empty":
				_ = os.WriteFile(b.CredentialFile, []byte(" \n"), 0600)
			case "embedded-space":
				_ = os.WriteFile(b.CredentialFile, []byte("first second"), 0600)
			case "embedded-newline":
				_ = os.WriteFile(b.CredentialFile, []byte("first\nsecond"), 0600)
			case "control":
				_ = os.WriteFile(b.CredentialFile, []byte("first\x00second"), 0600)
			case "symlink":
				if runtime.GOOS == "windows" {
					t.Skip("native Unix symlink fixture")
				}
				link := filepath.Join(t.TempDir(), "linked.token")
				if err := os.Symlink(b.CredentialFile, link); err != nil {
					t.Fatal(err)
				}
				b.CredentialFile = link
			case "permissions":
				if runtime.GOOS == "windows" {
					t.Skip("Unix mode permissions")
				}
				if err := os.Chmod(b.CredentialFile, 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := client.New(context.Background(), b)
			code := "credential_invalid"
			if kind == "missing" {
				code = "credential_unreadable"
			}
			checkError(t, err, code, b.CredentialFile)
		})
	}
	if runtime.GOOS == "linux" {
		b := binding(t, server.URL)
		b.CredentialFile = "/dev/null"
		_, err := client.New(context.Background(), b)
		checkError(t, err, "credential_invalid", b.CredentialFile)
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe credential made request")
	}
}

func TestCredentialFIFORejectionIsExternallyBounded(t *testing.T) {
	if len(os.Args) >= 2 && os.Args[len(os.Args)-2] == "--fifo-credential-child" {
		b := profile.Binding{Context: profile.Context{Profile: "chosen", ServerURL: "http://127.0.0.1:1", InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: os.Args[len(os.Args)-1]}
		_, err := client.New(context.Background(), b)
		checkError(t, err, "credential_invalid", b.CredentialFile)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux named pipe proof")
	}
	path := filepath.Join(t.TempDir(), "private.fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if data, err := exec.CommandContext(ctx, "mkfifo", path).CombinedOutput(); err != nil {
		t.Fatalf("FIFO setup: %v %s", err, data)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCredentialFIFORejectionIsExternallyBounded$", "-test.timeout=2s", "--", "--fifo-credential-child", path)
	child.WaitDelay = time.Second
	if data, err := child.CombinedOutput(); err != nil {
		t.Fatalf("FIFO rejection exceeded bound or failed: %v %s", err, data)
	}
}

func TestOmittedLimitHonorsCalibratableServerDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.URL.Query().Has("limit") {
			t.Error("omitted limit became a client default")
		}
		items := make([]client.Worker, 25)
		for i := range items {
			items[i] = client.Worker{ID: core.NewID("worker"), ProjectID: "project-one", Name: "Builder"}
		}
		write(w, 200, client.Page[client.Worker]{Items: items, Cursor: "next-server-page", HasMore: true})
	}))
	defer server.Close()
	c := newClient(t, server)
	value, err := c.Workers(context.Background(), client.PageOptions{})
	if err != nil || len(value.Items) != 25 || value.Cursor != "next-server-page" || !value.HasMore {
		t.Fatalf("server default page lost completeness: %v", err)
	}
}

func TestServerReplacementAfterHandshakeIsRejectedByRealGuard(t *testing.T) {
	var providers atomic.Int64
	type fixture struct {
		store                        *core.Store
		handler                      *http.ServeMux
		admin, path, project, worker string
	}
	invoke := func(handler http.Handler, token, route string, body any) map[string]any {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", route, strings.NewReader(string(data)))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", core.NewID("key"))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("synthetic service setup status=%d", response.Code)
		}
		var value map[string]any
		if err = json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	open := func() fixture {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "private")
		store, err := core.Open(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		path := filepath.Join(dir, "bootstrap-admin.token")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		admin := strings.TrimSpace(string(data))
		mux := server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
			providers.Add(1)
			return server.RepositoryIdentity{ID: 1, FullName: "owner/repository"}, nil
		}))
		project := invoke(mux, admin, "/api/v1/projects", map[string]string{"name": "project", "repository": "owner/repository"})["id"].(string)
		worker := invoke(mux, admin, "/api/v1/projects/"+project+"/workers", map[string]string{"name": "Builder"})["id"].(string)
		return fixture{store, mux, admin, path, project, worker}
	}
	first, second := open(), open()
	sum := sha256.Sum256([]byte(first.admin))
	invoke(second.handler, second.admin, "/api/v1/projects/"+second.project+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	var replaced atomic.Bool
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if replaced.Load() {
			second.handler.ServeHTTP(w, r)
		} else {
			first.handler.ServeHTTP(w, r)
		}
	}))
	defer httpServer.Close()
	b := profile.Binding{Context: profile.Context{Profile: "chosen", ServerURL: httpServer.URL, InstanceID: first.store.InstanceID(), ProjectID: first.project, WorkerID: first.worker}, CredentialFile: first.path}
	c, err := client.New(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if worker, err := c.Worker(context.Background()); err != nil || worker.ID != first.worker {
		t.Fatalf("initial scoped read failed: %v", err)
	}
	calls := providers.Load()
	replaced.Store(true)
	_, err = c.Worker(context.Background())
	checkError(t, err, "instance_mismatch", first.path)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Status != 409 || providers.Load() != calls {
		t.Fatal("replacement guard lost conflict or performed provider work")
	}
}

func TestOpenJobListClientFilterAndContinuation(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		reads.Add(1)
		if r.Method != "GET" || r.URL.Query().Get("status") != "open" || r.URL.Query().Get("worker_id") != "worker-one" || r.URL.Query().Get("limit") != "1" {
			t.Error("open filter or read semantics lost")
		}
		if r.URL.Query().Get("cursor") == "" {
			write(w, 200, client.Page[client.JobSummary]{Items: []client.JobSummary{{ID: "job-a", Title: "A", Status: "active", WorkerID: "worker-one", Revision: 1}}, HasMore: true, Cursor: "signed-next"})
		} else {
			if r.URL.Query().Get("cursor") != "signed-next" {
				t.Error("continuation changed")
			}
			write(w, 200, client.Page[client.JobSummary]{Items: []client.JobSummary{{ID: "job-b", Title: "B", Status: "parked", WorkerID: "worker-one", Revision: 1}}})
		}
	}))
	defer service.Close()
	c := newClient(t, service)
	page, err := c.Jobs(t.Context(), client.PageOptions{Limit: 1}, client.JobFilter{Status: "open", WorkerID: "worker-one"})
	if err != nil || !page.HasMore || len(page.Items) != 1 || page.Items[0].Status != "active" {
		t.Fatal("first open page", err)
	}
	page, err = c.Jobs(t.Context(), client.PageOptions{Limit: 1, Cursor: page.Cursor}, client.JobFilter{Status: "open", WorkerID: "worker-one"})
	if err != nil || page.HasMore || page.Items[0].Status != "parked" || reads.Load() != 2 {
		t.Fatal("second open page", err)
	}
	if !client.ValidJobStatusFilter("open") || client.ValidStatus("open") {
		t.Fatal("list-only validation changed actual states")
	}
	before := reads.Load()
	_, err = c.CreateJob(t.Context(), "invalid-open", client.CreateJobRequest{Title: "Cannot store filter", Status: "open"})
	checkError(t, err, "validation_failed")
	if reads.Load() != before {
		t.Fatal("open mutation dispatched")
	}
}

func TestOpenJobListClientRejectsMismatchedResponses(t *testing.T) {
	for _, tc := range []struct{ status, worker string }{{"completed", "worker-one"}, {"open", "worker-one"}, {"active", "worker-other"}} {
		t.Run(tc.status+tc.worker, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, client.Page[client.JobSummary]{Items: []client.JobSummary{{ID: "job-one", Title: "Task", WorkerID: tc.worker, Status: tc.status, Revision: 1}}})
			}))
			defer service.Close()
			c := newClient(t, service)
			_, err := c.Jobs(t.Context(), client.PageOptions{}, client.JobFilter{Status: "open", WorkerID: "worker-one"})
			checkError(t, err, "invalid_response")
		})
	}
}
