package mcpadapter_test

import (
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

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func apiRequest(t *testing.T, handler http.Handler, method, path, token, key, worker, body string, want int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return value
}

func TestInboxSnapshotAndEventsToolsUseScopedCommittedReplay(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = jobs.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err = inbox.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	handler := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 4321, FullName: repository}, nil
	}))
	jobs.Register(handler, store)
	inbox.Register(handler, store)
	adminBytes, err := os.ReadFile(filepath.Join(dataDir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	adminToken := strings.TrimSpace(string(adminBytes))

	projectReceipt := apiRequest(t, handler, http.MethodPost, "/api/v1/projects", adminToken, "mcp-project-create", "", `{"name":"MCP events","repository":"owner/mcp-events"}`, http.StatusOK)
	projectID := projectReceipt["id"].(string)
	createWorker := func(name, key string) string {
		receipt := apiRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/workers", adminToken, key, "", `{"name":"`+name+`"}`, http.StatusOK)
		return receipt["id"].(string)
	}
	sender := createWorker("Builder", "mcp-worker-builder")
	recipient := createWorker("Reviewer", "mcp-worker-reviewer")
	projectToken := "synthetic-mcp-events-project-token-123456"
	hash := sha256.Sum256([]byte(projectToken))
	apiRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/credentials", adminToken, "mcp-project-grant", "", `{"token_sha256":"`+hex.EncodeToString(hash[:])+`","permissions":["read","write"]}`, http.StatusOK)

	waitStarted := make(chan struct{}, 1)
	waitCanceled := make(chan struct{}, 1)
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			http.Error(w, "writes are not available in this test", http.StatusMethodNotAllowed)
			return
		}
		waiting := strings.HasSuffix(r.URL.Path, "/message-events") && r.URL.Query().Get("wait_ms") == "3000"
		if waiting {
			select {
			case waitStarted <- struct{}{}:
			default:
			}
		}
		handler.ServeHTTP(w, r)
		if waiting && r.Context().Err() != nil {
			select {
			case waitCanceled <- struct{}{}:
			default:
			}
		}
	}))
	defer service.Close()
	profileContext := profile.Context{Profile: "events", ServerURL: service.URL, InstanceID: store.InstanceID(), ProjectID: projectID, WorkerID: recipient}
	credentialFile := filepath.Join(t.TempDir(), "events.token")
	if err = os.WriteFile(credentialFile, []byte(projectToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profileFile := profiles(t, map[string]profile.Context{"events": profileContext}, credentialFile)
	session := connect(t, adapter(t, profileFile, "events"), "")

	snapshotResult := call(t, session, "inbox_snapshot", map[string]any{"limit": 5, "unread": true, "unacknowledged": true})
	var snapshot struct {
		Inbox struct {
			Items  []any  `json:"items"`
			Cursor string `json:"cursor"`
		} `json:"inbox"`
		EventCursor string `json:"event_cursor"`
	}
	decodeResult(t, snapshotResult, &snapshot)
	if len(snapshot.Inbox.Items) != 0 || snapshot.EventCursor == "" {
		t.Fatalf("unexpected initial snapshot: %#v", snapshot)
	}

	message := apiRequest(t, handler, http.MethodPost, "/api/v1/projects/"+projectID+"/messages", projectToken, "mcp-message-send", sender, `{"recipient_worker_id":"`+recipient+`","body":"private event body sentinel"}`, http.StatusCreated)
	messageID := message["id"].(string)
	eventsResult := call(t, session, "inbox_events", map[string]any{"cursor": snapshot.EventCursor, "limit": 1, "wait_ms": 4000})
	var events struct {
		Items []struct {
			Sequence   int64          `json:"sequence"`
			ProjectID  string         `json:"project_id"`
			Type       string         `json:"type"`
			ResourceID string         `json:"resource_id"`
			Payload    map[string]any `json:"payload"`
		} `json:"items"`
		Cursor   string `json:"cursor"`
		HasMore  bool   `json:"has_more"`
		TimedOut bool   `json:"timed_out"`
	}
	decodeResult(t, eventsResult, &events)
	if len(events.Items) != 1 || events.Items[0].Sequence < 1 || events.Items[0].ProjectID != projectID || events.Items[0].Type != "message.sent" || events.Items[0].ResourceID != messageID || events.Items[0].Payload["recipient_worker_id"] != recipient || events.Cursor == "" || events.HasMore || events.TimedOut {
		t.Fatalf("unexpected event replay: %#v", events)
	}
	encoded, _ := json.Marshal(eventsResult.StructuredContent)
	if strings.Contains(string(encoded), "private event body sentinel") {
		t.Fatal("event tool exposed a message body")
	}

	inboxPage := apiRequest(t, handler, http.MethodGet, "/api/v1/projects/"+projectID+"/inbox/"+recipient, projectToken, "", recipient, "", http.StatusOK)
	items := inboxPage["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("inbox item missing after event read: %#v", inboxPage)
	}
	item := items[0].(map[string]any)
	if item["id"] != messageID || item["seen"] != false || item["acknowledged"] != false {
		t.Fatalf("snapshot/event read changed receipt state: %#v", item)
	}
	if writes.Load() != 0 {
		t.Fatalf("MCP read tools made %d HTTP writes", writes.Load())
	}

	// A pending bounded wait must not serialize another read on the same SDK
	// session. Cancellation must release it so the session remains usable.
	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, callErr := session.CallTool(waitCtx, &mcp.CallToolParams{Name: "inbox_events", Arguments: map[string]any{"cursor": events.Cursor, "wait_ms": 3000}})
		waitDone <- callErr
	}()
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		cancelWait()
		t.Fatal("bounded MCP event wait did not reach the service")
	}
	concurrentSnapshot := call(t, session, "inbox_snapshot", map[string]any{"limit": 1})
	if concurrentSnapshot.IsError {
		cancelWait()
		t.Fatal("concurrent snapshot failed while event wait was pending")
	}
	cancelWait()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled MCP event wait did not return")
	}
	select {
	case <-waitCanceled:
	case <-time.After(time.Second):
		t.Fatal("MCP cancellation did not reach the bounded service wait")
	}
	if writes.Load() != 0 {
		t.Fatalf("MCP cancellation/read made %d HTTP writes", writes.Load())
	}
}
