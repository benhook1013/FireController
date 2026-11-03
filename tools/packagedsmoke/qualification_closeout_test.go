//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
)

// A small simultaneous workload, not a fleet benchmark or host wakeup claim.
// Each project has twelve identically named workers with separate identities.
func TestPackagedQualificationCoordination(t *testing.T) {
	started := time.Now()
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(stop)
	private := privateFixtureDirectory(t)
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute input directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fcd := filepath.Join(binDir, "fcd"+suffix)
	before := binaryHash(t, fcd)
	t.Cleanup(func() {
		if before != binaryHash(t, fcd) {
			t.Error("supplied daemon changed")
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		external.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, name := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap unavailable")
	}
	bootstrap.stop(t)
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped fixture unavailable")
	}
	mux := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repo string) (server.RepositoryIdentity, error) {
		id := int64(111)
		if repo == "synthetic/second" {
			id++
		}
		return server.RepositoryIdentity{ID: id, FullName: repo}, nil
	}))
	seed := func(path string, value any) string {
		t.Helper()
		body, _ := json.Marshal(value)
		r := httptest.NewRequest("POST", path, bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(admin)))
		r.Header.Set("X-FC-Instance", bootstrap.instance)
		r.Header.Set("Idempotency-Key", core.NewID("qualification"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var receipt struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
			t.Fatal("trusted setup refused")
		}
		return receipt.ID
	}
	type identity struct{ project, worker, credential string }
	var identities []identity
	for _, repo := range []string{"synthetic/first", "synthetic/second"} {
		project := seed("/api/v1/projects", map[string]any{"name": repo, "repository": repo})
		prefix := "/api/v1/projects/" + project
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			t.Fatal("synthetic credential unavailable")
		}
		token := hex.EncodeToString(secret)
		digest := sha256.Sum256([]byte(token))
		seed(prefix+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "permissions": []string{"read", "write"}})
		credential := filepath.Join(private, project+".token")
		if err = os.WriteFile(credential, []byte(token+"\n"), 0600); err != nil {
			t.Fatal("private credential unavailable")
		}
		for index := range 12 {
			worker := seed(prefix+"/workers", map[string]any{"name": fmt.Sprintf("Worker %02d", index)})
			identities = append(identities, identity{project, worker, credential})
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal("fixture close failed")
	}
	daemon := startDaemon(t, ctx, fcd, data, env)
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxyTransport := &http.Transport{Proxy: nil}
	proxy.Transport = proxyTransport
	t.Cleanup(proxyTransport.CloseIdleConnections)
	proxy.FlushInterval = -1
	opened := make(chan struct{}, len(identities))
	proxy.ModifyResponse = func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/message-events/stream") && r.StatusCode == 200 {
			select {
			case opened <- struct{}{}:
			default:
			}
		}
		return nil
	}
	gateway := httptest.NewServer(proxy)
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	clients := make([]*client.Client, len(identities))
	cursors := make([]string, len(identities))
	for i, id := range identities {
		clients[i], err = client.New(ctx, profile.Binding{Context: profile.Context{Profile: "qualification", ServerURL: gateway.URL, InstanceID: daemon.instance, ProjectID: id.project, WorkerID: id.worker}, CredentialFile: id.credential})
		if err != nil {
			t.Fatal("bound client unavailable")
		}
		t.Cleanup(clients[i].Close)
		snapshot, readErr := clients[i].InboxSnapshot(ctx, client.PageOptions{}, client.InboxFilter{})
		if readErr != nil || len(snapshot.Inbox.Items) != 0 {
			t.Fatal("initial inbox invalid")
		}
		cursors[i] = snapshot.EventCursor
	}
	type result struct {
		index int
		page  client.MessageEventPage
		err   error
	}
	results := make(chan result, len(clients))
	waitCtx, cancelWaits := context.WithCancel(ctx)
	t.Cleanup(cancelWaits)
	var waiters sync.WaitGroup
	for i, c := range clients {
		waiters.Add(1)
		go func(i int, c *client.Client) {
			defer waiters.Done()
			page, waitErr := c.WaitForMessages(waitCtx, cursors[i], 20, 15)
			results <- result{i, page, waitErr}
		}(i, c)
	}
	t.Cleanup(func() { cancelWaits(); waiters.Wait() })
	for range clients {
		select {
		case <-opened:
		case <-ctx.Done():
			t.Fatal("all concurrent streams did not establish")
		}
	}
	workStarted := time.Now()
	jobs := make([]client.JobReceipt, len(clients))
	messages := make([]client.MessageReceipt, len(clients))
	failures := make(chan error, len(clients))
	var writers sync.WaitGroup
	for i, c := range clients {
		writers.Add(1)
		go func(i int, c *client.Client) {
			defer writers.Done()
			job, createErr := c.CreateJob(ctx, fmt.Sprintf("qualification-job-%d", i), client.CreateJobRequest{Title: "Concurrent job", Brief: "Private working context", WorkerID: identities[i].worker})
			if createErr != nil {
				failures <- createErr
				return
			}
			jobs[i] = job
			next := i/12*12 + (i+1)%12
			request := client.SendMessageRequest{RecipientWorkerID: identities[next].worker, Body: "Concurrent coordination"}
			message, sendErr := c.SendMessage(ctx, fmt.Sprintf("qualification-message-%d", i), request)
			if sendErr != nil {
				failures <- sendErr
				return
			}
			messages[i] = message
			replay, replayErr := c.SendMessage(ctx, fmt.Sprintf("qualification-message-%d", i), request)
			if replayErr != nil || replay != message {
				failures <- fmt.Errorf("exact replay mismatch")
				return
			}
			read, readErr := c.Job(ctx, job.ID)
			if readErr != nil || read.ID != job.ID || read.WorkerID != identities[i].worker {
				failures <- fmt.Errorf("concurrent job read mismatch")
			}
		}(i, c)
	}
	writers.Wait()
	close(failures)
	for failure := range failures {
		if service, ok := failure.(*client.Error); ok {
			t.Errorf("concurrent operation failed: code=%s status=%d", service.Code, service.Status)
		} else {
			t.Error("concurrent operation failed")
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	for range clients {
		var value result
		select {
		case value = <-results:
		case <-ctx.Done():
			t.Fatal("concurrent delivery exceeded budget")
		}
		previous := value.index/12*12 + (value.index+11)%12
		if value.err != nil || len(value.page.Items) != 1 || value.page.Items[0].ResourceID != messages[previous].ID || value.page.Items[0].ProjectID != identities[value.index].project || value.page.TimedOut {
			t.Fatal("delivery lost, duplicated or crossed project")
		}
	}
	waiters.Wait()
	for i, c := range clients {
		snapshot, readErr := c.InboxSnapshot(ctx, client.PageOptions{}, client.InboxFilter{})
		if readErr != nil || len(snapshot.Inbox.Items) != 1 || snapshot.Inbox.Items[0].Seen || snapshot.Inbox.Items[0].Acknowledged {
			t.Fatal("wait acknowledged or duplicated a message")
		}
		otherProject := (i + 12) % len(clients)
		if _, readErr = c.Job(ctx, jobs[otherProject].ID); readErr == nil {
			t.Fatal("cross-project job read succeeded")
		}
	}
	if external.Load() != 0 {
		t.Fatal("unexpected provider traffic")
	}
	for _, c := range clients {
		c.Close()
	}
	gateway.CloseClientConnections()
	gateway.Close()
	proxyTransport.CloseIdleConnections()
	daemon.stop(t)
	t.Logf("qualification passed projects=2 workers=24 simultaneous_waits=24 jobs=24 original_messages=24 exact_replays=24 deliveries=24 cross_project_refusals=24 external=0 workload=%s total=%s daemon_sha256=%x", time.Since(workStarted).Round(time.Millisecond), time.Since(started).Round(time.Millisecond), before)
}
