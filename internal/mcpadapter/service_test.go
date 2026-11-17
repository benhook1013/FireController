package mcpadapter_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const token = "synthetic-readonly-adapter-credential-12345"

func jsonResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func credential(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.token")
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAllToolsReadRealScopedAPIWithoutProvidersOrMutations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = inbox.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err = jobs.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	var providers, reads atomic.Int64
	mux := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repo string) (server.RepositoryIdentity, error) {
		id := providers.Add(1)
		return server.RepositoryIdentity{ID: id, FullName: repo}, nil
	}))
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	adminData, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(string(adminData))
	invoke := func(method, route, worker string, body any) map[string]any {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, route, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("Idempotency-Key", core.NewID("key"))
		if worker != "" {
			r.Header.Set("X-FC-Worker", worker)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("fixture setup HTTP %d: %s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	p := invoke("POST", "/api/v1/projects", "", map[string]string{"name": "Alpha", "repository": "example/alpha"})["id"].(string)
	q := invoke("POST", "/api/v1/projects", "", map[string]string{"name": "Beta", "repository": "example/beta"})["id"].(string)
	base := "/api/v1/projects/" + p
	worker := invoke("POST", base+"/workers", "", map[string]string{"name": "Builder"})["id"].(string)
	sender := invoke("POST", base+"/workers", "", map[string]string{"name": "Sender"})["id"].(string)
	otherWorker := invoke("POST", "/api/v1/projects/"+q+"/workers", "", map[string]string{"name": "Builder"})["id"].(string)
	for _, project := range []string{p, q} {
		credentialValue := token
		if project == q {
			credentialValue += "-beta"
		}
		sum := sha256.Sum256([]byte(credentialValue))
		invoke("POST", "/api/v1/projects/"+project+"/credentials", "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	}
	brief := "Explicit full private brief"
	job := invoke("POST", base+"/jobs", "", map[string]string{"title": "First", "brief": brief, "worker_id": worker})["id"].(string)
	job2 := invoke("POST", base+"/jobs", "", map[string]string{"title": "Second", "brief": brief, "worker_id": worker})["id"].(string)
	invoke("POST", base+"/jobs", "", map[string]string{"title": "Sender only", "worker_id": sender})
	invoke("POST", "/api/v1/projects/"+q+"/jobs", "", map[string]string{"title": "Beta private", "worker_id": otherWorker})
	invoke("PATCH", base+"/jobs/"+job, "", map[string]any{"expected_revision": 1, "title": "First revised"})
	body := strings.Repeat("private message ", 20)
	message := invoke("POST", base+"/messages", sender, map[string]string{"recipient_worker_id": worker, "body": body})
	conversation := message["conversation_id"].(string)
	invoke("POST", base+"/messages", worker, map[string]string{"recipient_worker_id": sender, "body": "Reply full body", "reply_to_message_id": message["id"].(string)})
	invoke("POST", base+"/messages", sender, map[string]string{"recipient_worker_id": worker, "body": "Second incoming"})
	counts := func() []int {
		t.Helper()
		out := make([]int, 3)
		for i, table := range []string{"events", "receipts", "job_history"} {
			if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := counts()
	providerBefore := providers.Load()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Method != "GET" {
			t.Error("MCP requested mutation")
		}
		if r.Header.Get("X-FC-Instance") != store.InstanceID() {
			t.Error("expected instance missing")
		}
		if r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") == "" {
			t.Error("bound worker missing")
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	a := profile.Context{Profile: "alpha", ServerURL: service.URL, InstanceID: store.InstanceID(), ProjectID: p, WorkerID: worker}
	b := a
	b.Profile = "beta"
	b.ProjectID = q
	b.WorkerID = otherWorker
	credentialPath := credential(t)
	path := profiles(t, map[string]profile.Context{"alpha": a, "beta": b}, credentialPath)
	cs := connect(t, adapter(t, path, "alpha"), "")
	if reads.Load() != 0 {
		t.Fatal("initialize contacted service")
	}
	if _, err = cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 0 {
		t.Fatal("discovery contacted service")
	}
	var doctor struct {
		profile.Context
		Verified bool `json:"verified"`
	}
	decodeResult(t, call(t, cs, "doctor", nil), &doctor)
	if !doctor.Verified || doctor.Context != a {
		t.Fatal("doctor identity")
	}
	var workers client.Page[client.Worker]
	decodeResult(t, call(t, cs, "workers_list", map[string]any{"limit": 1}), &workers)
	if len(workers.Items) != 1 || !workers.HasMore || workers.Cursor == "" {
		t.Fatal("worker page lost continuation")
	}
	var nextWorkers client.Page[client.Worker]
	decodeResult(t, call(t, cs, "workers_list", map[string]any{"limit": 1, "cursor": workers.Cursor}), &nextWorkers)
	if len(nextWorkers.Items) != 1 || nextWorkers.HasMore || nextWorkers.Items[0].ID == workers.Items[0].ID {
		t.Fatal("worker pages incomplete")
	}
	var page client.Page[client.JobSummary]
	decodeResult(t, call(t, cs, "jobs_assigned", map[string]any{"limit": 1, "status": "active"}), &page)
	if len(page.Items) != 1 || !page.HasMore || page.Items[0].WorkerID != worker {
		t.Fatal("assigned page")
	}
	var next client.Page[client.JobSummary]
	decodeResult(t, call(t, cs, "jobs_assigned", map[string]any{"limit": 1, "status": "active", "cursor": page.Cursor}), &next)
	if len(next.Items) != 1 || next.HasMore || next.Items[0].ID == page.Items[0].ID || next.Items[0].WorkerID != worker {
		t.Fatal("assigned continuation")
	}
	ids := map[string]bool{page.Items[0].ID: true, next.Items[0].ID: true}
	if !ids[job] || !ids[job2] {
		t.Fatal("assigned identities changed")
	}
	summaryJSON, _ := json.Marshal(page)
	if strings.Contains(string(summaryJSON), brief) {
		t.Fatal("compact list returned full brief")
	}
	var filtered client.Page[client.JobSummary]
	decodeResult(t, call(t, cs, "jobs_list", map[string]any{"worker_id": sender, "status": "active"}), &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].WorkerID != sender {
		t.Fatal("explicit filter ignored")
	}
	var detail client.Job
	decodeResult(t, call(t, cs, "job_read", map[string]any{"job_id": job}), &detail)
	if detail.Brief != brief || detail.Revision != 2 || detail.ProjectID != p {
		t.Fatal("explicit detail lost data")
	}
	var history client.Page[client.JobHistoryEntry]
	decodeResult(t, call(t, cs, "job_history", map[string]any{"job_id": job, "limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 1 || !history.HasMore || history.Items[0].State.Brief != brief {
		t.Fatal("history page")
	}
	var history2 client.Page[client.JobHistoryEntry]
	decodeResult(t, call(t, cs, "job_history", map[string]any{"job_id": job, "limit": 1, "cursor": history.Cursor}), &history2)
	if len(history2.Items) != 1 || history2.HasMore || history2.Items[0].Revision != 2 {
		t.Fatal("history continuation")
	}
	var inboxPage client.Page[client.InboxItem]
	decodeResult(t, call(t, cs, "inbox_list", map[string]any{"unread": true, "unacknowledged": true, "limit": 1}), &inboxPage)
	if len(inboxPage.Items) != 1 || !inboxPage.HasMore || !inboxPage.Items[0].PreviewTruncated || inboxPage.Items[0].Seen || inboxPage.Items[0].Acknowledged {
		t.Fatal("compact inbox changed receipts")
	}
	if strings.Contains(inboxPage.Items[0].Preview, body) {
		t.Fatal("compact inbox full body")
	}
	var inboxNext client.Page[client.InboxItem]
	decodeResult(t, call(t, cs, "inbox_list", map[string]any{"unread": true, "unacknowledged": true, "limit": 1, "cursor": inboxPage.Cursor}), &inboxNext)
	if len(inboxNext.Items) != 1 || inboxNext.HasMore || inboxNext.Items[0].ID == inboxPage.Items[0].ID || inboxNext.Items[0].Preview != "Second incoming" {
		t.Fatal("inbox continuation incomplete")
	}
	var thread client.ConversationPage
	decodeResult(t, call(t, cs, "inbox_thread", map[string]any{"conversation_id": conversation, "limit": 1}), &thread)
	if len(thread.Items) != 1 || !thread.HasMore || thread.Items[0].Body != body {
		t.Fatal("thread full body/page")
	}
	var thread2 client.ConversationPage
	decodeResult(t, call(t, cs, "inbox_thread", map[string]any{"conversation_id": conversation, "limit": 1, "cursor": thread.Cursor}), &thread2)
	if len(thread2.Items) != 1 || thread2.HasMore || thread2.Items[0].Body != "Reply full body" {
		t.Fatal("thread continuation")
	}
	betaCredential := filepath.Join(t.TempDir(), "beta.token")
	if err = os.WriteFile(betaCredential, []byte(token+"-beta"), 0600); err != nil {
		t.Fatal(err)
	}
	betaPath := profiles(t, map[string]profile.Context{"beta": b}, betaCredential)
	beta := connect(t, adapter(t, betaPath, "beta"), "")
	var betaJobs client.Page[client.JobSummary]
	decodeResult(t, call(t, beta, "jobs_assigned", nil), &betaJobs)
	if len(betaJobs.Items) != 1 || betaJobs.Items[0].Title != "Beta private" {
		t.Fatal("profiles mixed project data")
	}
	errorResult(t, call(t, beta, "job_read", map[string]any{"job_id": job}), "not_found", credentialPath, token)
	errorResult(t, call(t, beta, "jobs_assigned", map[string]any{"limit": 1, "status": "active", "cursor": page.Cursor}), "validation_failed", credentialPath, token)
	after := counts()
	for i := range before {
		if before[i] != after[i] {
			t.Fatal("read tools changed durable state")
		}
	}
	var seen, ack int
	if err = store.DB().QueryRow("SELECT sum(seen),sum(acknowledged) FROM inbox_recipient_state").Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("MCP marked inbox receipt")
	}
	if providers.Load() != providerBefore {
		t.Fatal("normal MCP use performed provider work")
	}
	if reads.Load() != 32 {
		t.Fatalf("unexpected handshake/read count or retry: %d", reads.Load())
	}
}

func TestCancellationReachesHTTPAndSessionRemainsUsable(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": "instance-one"})
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(8 * time.Second):
		}
		close(canceled)
	}))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, credential(t))
	cs := connect(t, adapter(t, path, "alpha"), "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "doctor", Arguments: map[string]any{}})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SDK call did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP did not cancel")
	}
	var got profile.Context
	decodeResult(t, call(t, cs, "context", nil), &got)
	if got != c {
		t.Fatal("cancellation broke session")
	}
}

func TestOperationDeadlineAfterHandshakeReturnsSafeToolError(t *testing.T) {
	started := time.Now()
	var readStarted atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			select {
			case <-time.After(600 * time.Millisecond):
				jsonResponse(w, map[string]string{"instance_id": "instance-one"})
			case <-r.Context().Done():
			}
			return
		}
		readStarted.Store(time.Since(started).Nanoseconds())
		select {
		case <-r.Context().Done():
		case <-time.After(8 * time.Second):
		}
	}))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	private := credential(t)
	path := profiles(t, map[string]profile.Context{"alpha": c}, private)
	cs := connect(t, adapter(t, path, "alpha"), "")
	started = time.Now()
	result := call(t, cs, "doctor", nil)
	elapsed := time.Since(started)
	errorResult(t, result, "deadline_exceeded", private, token)
	if readStarted.Load() == 0 || elapsed < 5400*time.Millisecond || elapsed > 6500*time.Millisecond {
		t.Fatalf("operation deadline after bounded identity read: %s", elapsed)
	}
}

func TestSafeServiceFailuresAndInstanceMismatch(t *testing.T) {
	private := credential(t)
	for _, kind := range []string{"mismatch", "reflected", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if kind == "mismatch" {
					jsonResponse(w, map[string]string{"instance_id": "different"})
					return
				}
				if r.URL.Path == "/api/v1/instance" {
					jsonResponse(w, map[string]string{"instance_id": "instance-one"})
					return
				}
				message := "private " + token + " " + private
				if kind == "oversized" {
					message = strings.Repeat("x", 600)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				jsonResponse(w, map[string]any{"error": map[string]string{"code": "forbidden", "message": message}, "request_id": "request-one"})
			}))
			defer service.Close()
			c := localContext()
			c.ServerURL = service.URL
			path := profiles(t, map[string]profile.Context{"alpha": c}, private)
			result := call(t, connect(t, adapter(t, path, "alpha"), ""), "doctor", nil)
			code := "invalid_response"
			if kind == "mismatch" {
				code = "instance_mismatch"
			}
			errorResult(t, result, code, private, token)
			if kind == "mismatch" && calls.Load() != 1 {
				t.Fatal("read continued after mismatch")
			}
		})
	}
}
