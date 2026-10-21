package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/server"
)

type output struct {
	sync.Mutex
	data  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (o *output) Write(data []byte) (int, error) {
	o.Lock()
	defer o.Unlock()
	n, err := o.data.Write(data)
	if bytes.Contains(o.data.Bytes(), []byte("\n")) {
		o.once.Do(func() { close(o.ready) })
	}
	return n, err
}
func (o *output) String() string { o.Lock(); defer o.Unlock(); return o.data.String() }

func TestExecutableBootIdentityAndGracefulRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	instance := ""
	for _, listen := range []string{"127.0.0.1:0", "localhost:0"} {
		ctx, cancel := context.WithCancel(context.Background())
		stdout := &output{ready: make(chan struct{})}
		stderr := &output{ready: make(chan struct{})}
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"--data-dir", dir, "--listen", listen}, stdout, stderr) }()
		select {
		case <-stdout.ready:
		case code := <-done:
			cancel()
			t.Fatalf("startup stopped: code=%d diagnostics=%s", code, stderr.String())
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("startup deadline")
		}
		var started map[string]string
		if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil {
			cancel()
			t.Fatal(err)
		}
		host, _, err := net.SplitHostPort(started["address"])
		if err != nil || host != "127.0.0.1" {
			cancel()
			t.Fatalf("listener %s did not bind pinned IPv4 loopback: %s", listen, started["address"])
		}
		if instance == "" {
			instance = started["instance_id"]
		} else if instance != started["instance_id"] {
			cancel()
			t.Fatal("restart changed instance")
		}
		transport := &http.Transport{DisableKeepAlives: true}
		client := &http.Client{Timeout: time.Second, Transport: transport}
		response, err := client.Get("http://" + started["address"] + "/readyz")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			cancel()
			t.Fatal("not ready")
		}
		for _, route := range []string{
			"/api/v1/projects/untrusted/jobs",
			"/api/v1/projects/untrusted/inbox/untrusted",
			"/api/v1/projects/untrusted/reviews/queue",
			"/api/v1/projects/untrusted/reviews/operations/review_operation_" + strings.Repeat("a", 32),
			"/api/v1/projects/untrusted/reviews/evidence/review_evidence_" + strings.Repeat("a", 32),
			"/api/v1/projects/untrusted/reviews/evidence/review_evidence_" + strings.Repeat("a", 32) + "/adjudication",
			"/api/v1/projects/untrusted/reviews/pulls/1",
			"/api/v1/projects/untrusted/reviews/pulls/1/runs",
			"/api/v1/projects/untrusted/reviews/runs/review_run_" + strings.Repeat("a", 32),
		} {
			response, err = client.Get("http://" + started["address"] + route)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				cancel()
				t.Fatalf("domain route %s not registered with authentication: %d", route, response.StatusCode)
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		token := strings.TrimSpace(string(data))
		req, _ := http.NewRequest("GET", "http://"+started["address"]+"/api/v1/instance", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err = client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte(instance)) {
			cancel()
			t.Fatal("authenticated instance inspection failed")
		}
		transport.CloseIdleConnections()
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("shutdown code=%d diagnostics=%s", code, stderr.String())
			}
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown deadline")
		}
		if strings.Contains(stdout.String()+stderr.String(), token) || stderr.String() != "" {
			t.Fatal("startup/shutdown emitted credential or diagnostics")
		}
	}
}

type listenerAddress struct {
	net.Listener
	address net.Addr
}

func (l listenerAddress) Addr() net.Addr { return l.address }

func TestBoundListenerRejectsUnsafeOrUnexpectedAddressAndClosesSocket(t *testing.T) {
	for _, test := range []struct {
		name    string
		address net.Addr
	}{
		{"public TCP", &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8080}},
		{"unspecified TCP", &net.TCPAddr{IP: net.IPv4zero, Port: 8080}},
		{"unexpected address type", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}},
		{"missing TCP address", (*net.TCPAddr)(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if validateLoopbackListener(listenerAddress{Listener: listener, address: test.address}) {
				t.Fatal("unsafe bound address accepted")
			}
			if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("rejected listener socket was not closed: %v", err)
			}
		})
	}
}

func TestConfigurationRejectsNonLoopbackBeforeCreatingState(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "[::]:8080", "controller.example:8080", "127.0.0.1:65536", "127.0.0.1:bad"} {
		dir := filepath.Join(t.TempDir(), "absent")
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"--data-dir", dir, "--listen", address}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatal("unsafe listener accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid configuration created state")
		}
	}
}

func TestInvalidGitHubCredentialRejectsBeforeCreatingState(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "empty", "malformed", "nonprivate"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "nonprivate" && runtime.GOOS == "windows" {
				t.Skip("Unix permission bits are not Windows access-control proof")
			}
			root := t.TempDir()
			tokenPath := filepath.Join(root, "operator.token")
			switch kind {
			case "directory":
				if err := os.Mkdir(tokenPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "empty", "malformed", "nonprivate":
				data := ""
				if kind == "malformed" {
					data = "synthetic token with spaces"
				}
				if kind == "nonprivate" {
					data = "synthetic-private-token"
				}
				if err := os.WriteFile(tokenPath, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "nonprivate" {
					if err := os.Chmod(tokenPath, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			dataDir := filepath.Join(root, "must-remain-absent")
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"--data-dir", dataDir, "--listen", "127.0.0.1:0", "--github-token-file", tokenPath}, &stdout, &stderr)
			if code != 3 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code":"configuration_invalid"`) || strings.Contains(stderr.String(), tokenPath) || strings.Contains(stderr.String(), "synthetic") {
				t.Fatalf("invalid credential diagnostic or exit: code=%d", code)
			}
			if _, err := os.Lstat(dataDir); !os.IsNotExist(err) {
				t.Fatal("invalid optional credential created persistent state")
			}
		})
	}
}

func TestHTTPShutdownDrainsRequestsBeforeForcedClose(t *testing.T) {
	t.Run("request survives signal cancellation while draining", func(t *testing.T) {
		signalContext, cancelSignal := context.WithCancel(context.Background())
		defer cancelSignal()
		entered := make(chan struct{})
		release := make(chan struct{})
		requestCanceled := make(chan struct{})
		srv := newHTTPServer(signalContext, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-release:
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
				close(requestCanceled)
			}
		}))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serveDone := make(chan error, 1)
		go func() { serveDone <- srv.Serve(listener) }()
		responseDone := make(chan error, 1)
		go func() {
			response, requestErr := http.Get("http://" + listener.Addr().String())
			if requestErr == nil {
				_ = response.Body.Close()
			}
			responseDone <- requestErr
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request did not enter handler")
		}
		cancelSignal()
		select {
		case <-requestCanceled:
			t.Fatal("signal cancellation aborted the in-flight request")
		default:
		}
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- shutdownHTTPServer(srv, time.Second) }()
		select {
		case err := <-shutdownDone:
			t.Fatalf("shutdown did not wait for in-flight request: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-shutdownDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("graceful drain did not finish")
		}
		if err := <-responseDone; err != nil {
			t.Fatal(err)
		}
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve result: %v", err)
		}
	})

	t.Run("deadline forces close", func(t *testing.T) {
		signalContext, cancelSignal := context.WithCancel(context.Background())
		defer cancelSignal()
		entered := make(chan struct{})
		requestCanceled := make(chan struct{})
		srv := newHTTPServer(signalContext, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(requestCanceled)
		}))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = srv.Serve(listener) }()
		go func() {
			response, requestErr := http.Get("http://" + listener.Addr().String())
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request did not enter handler")
		}
		cancelSignal()
		started := time.Now()
		if err := shutdownHTTPServer(srv, 40*time.Millisecond); err == nil {
			t.Fatal("expected bounded shutdown to force-close stuck handler")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("bounded shutdown took too long: %s", elapsed)
		}
		select {
		case <-requestCanceled:
		case <-time.After(time.Second):
			t.Fatal("forced close did not cancel request")
		}
	})
}

func TestHTTPShutdownDetachesLongReviewWaitsAndKeepsAcceptedWork(t *testing.T) {
	f := newObserverTestFixture(t)
	lifetime, cancelWaits := context.WithCancel(context.Background())
	defer cancelWaits()
	capabilities := []reviewexec.Capability{
		{ID: "failed", Provider: "coderabbit", Account: "failed", Channels: []string{"hosted"}},
		{ID: "sibling", Provider: "coderabbit", Account: "sibling", Channels: []string{"hosted"}},
	}
	engine, err := reviewexec.New(f.store, capabilities, reviewexec.WithResultWaitLifetime(lifetime))
	if err != nil {
		t.Fatal(err)
	}
	mux := server.New(f.store)
	engine.Register(mux)
	srv := newHTTPServer(context.Background(), mux)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	defer func() {
		_ = srv.Close()
		_ = listener.Close()
	}()

	project := "failed"
	postPath := "/api/v1/projects/failed/reviews/requests"
	getReceipt, err := engine.Enqueue(t.Context(), f.scope(t, project), "shutdown-get-seed", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 1, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var getOperation reviewexec.Receipt
	if json.Unmarshal(getReceipt, &getOperation) != nil {
		t.Fatal("GET wait fixture receipt is invalid")
	}
	getPath := postPath[:len(postPath)-len("requests")] + "operations/" + getOperation.ID
	body, err := json.Marshal(reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 1, Channel: "hosted", HeadSHA: strings.Repeat("b", 40)})
	if err != nil {
		t.Fatal(err)
	}
	startRequest := func(method, path, key, data string) <-chan struct {
		status int
		body   string
	} {
		done := make(chan struct {
			status int
			body   string
		}, 1)
		go func() {
			request, requestErr := http.NewRequest(method, "http://"+listener.Addr().String()+path, strings.NewReader(data))
			if requestErr != nil {
				done <- struct {
					status int
					body   string
				}{0, requestErr.Error()}
				return
			}
			request.Header.Set("Authorization", "Bearer "+f.admin)
			request.Header.Set("X-FC-Instance", f.store.InstanceID())
			request.Header.Set("Prefer", "wait=900")
			if key != "" {
				request.Header.Set("Idempotency-Key", key)
			}
			response, requestErr := (&http.Client{Timeout: 8 * time.Second}).Do(request)
			if requestErr != nil {
				done <- struct {
					status int
					body   string
				}{0, requestErr.Error()}
				return
			}
			defer response.Body.Close()
			content, _ := io.ReadAll(response.Body)
			done <- struct {
				status int
				body   string
			}{response.StatusCode, string(content)}
		}()
		return done
	}
	postDone := startRequest(http.MethodPost, postPath, "shutdown-post", string(body))
	getDone := startRequest(http.MethodGet, getPath, "", "")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case result := <-postDone:
		t.Fatal("long POST returned before lifecycle shutdown", result.status, result.body)
	case result := <-getDone:
		t.Fatal("long GET returned before lifecycle shutdown", result.status, result.body)
	case <-time.After(40 * time.Millisecond):
	}
	started := time.Now()
	cancelWaits()
	if err = shutdownHTTPServer(srv, 5*time.Second); err != nil {
		t.Fatal("lifecycle waits forced HTTP shutdown", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatal("wait cancellation did not make daemon shutdown prompt", elapsed)
	}
	for index, done := range []<-chan struct {
		status int
		body   string
	}{postDone, getDone} {
		select {
		case result := <-done:
			var accepted reviewexec.Receipt
			postAccepted := index == 0 && result.status == http.StatusAccepted && json.Unmarshal([]byte(result.body), &accepted) == nil && accepted.State == "queued" && accepted.Revision == 1 && strings.HasPrefix(accepted.ID, "review_operation_") && !strings.Contains(result.body, "outcome_unknown")
			getCanceled := index == 1 && result.status == http.StatusServiceUnavailable && strings.Contains(result.body, `"code":"overloaded"`) && !strings.Contains(result.body, `"state":"completed"`)
			if !postAccepted && !getCanceled {
				t.Fatal("accepted wait returned unsafe shutdown response", result.status, result.body)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not release a result waiter")
		}
	}
	if err = <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("server did not finish graceful shutdown", err)
	}
	var queued, receipts int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE state='queued'`).Scan(&queued); err != nil || queued != 2 {
		t.Fatal("shutdown changed accepted operations", queued, err)
	}
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM receipts WHERE operation='reviews.request'`).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatal("shutdown changed accepted receipts", receipts, err)
	}
}
