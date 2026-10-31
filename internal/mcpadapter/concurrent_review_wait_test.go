package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConcurrentReviewWaitsDoNotBlockNativeMutation(t *testing.T) {
	f := mutationFixture(t)
	const parallel = 4
	started := make(chan string, parallel)
	detached := make(chan struct{}, parallel)
	var waiting, waitCalls, mutations, immediate atomic.Int64
	body := "One original message during concurrent review waits"
	wanted := client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: body}
	wantedJSON, err := json.Marshal(wanted)
	if err != nil {
		t.Fatal(err)
	}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews/operations/") {
			if r.Method != "GET" || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "" {
				t.Error("review read changed binding or action")
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/projects/"+f.project+"/reviews/operations/"), "/status")
			if boundedPositiveReviewWait(t, r.Header.Get("Prefer")) {
				waitCalls.Add(1)
				waiting.Add(1)
				started <- id
				<-r.Context().Done()
				waiting.Add(-1)
				detached <- struct{}{}
				return
			}
			immediate.Add(1)
			if r.Header.Get("Prefer") != "wait=0" {
				t.Error("unexpected immediate wait")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "cli", State: "queued", Revision: 1, Stage: "queued", Reason: "", AttemptID: "", UpdatedAt: "2026-01-01T00:00:00Z", NextAction: "wait"})
			return
		}
		if r.Method != "GET" {
			mutations.Add(1)
			raw, err := io.ReadAll(r.Body)
			if err != nil || r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+f.project+"/messages" || r.Header.Get("Idempotency-Key") != "concurrent-original-message" || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || !bytes.Equal(raw, wantedJSON) {
				t.Error("mutation changed exact request/key or binding")
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(service.Close)
	binding := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	// Own both ends so cancelled server replies drain before the client pipes
	// close. Client-side cancellation can return before the server writes them.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	lifetime, cancelLifetime := context.WithTimeout(t.Context(), 15*time.Second)
	adapterServer := adapter(t, profiles(t, map[string]profile.Context{"chosen": binding}, f.credential), "chosen")
	serverSession, err := adapterServer.Connect(lifetime, &mcp.IOTransport{Reader: inR, Writer: outW}, nil)
	if err != nil {
		cancelLifetime()
		t.Fatal(err)
	}
	sdk := mcp.NewClient(&mcp.Implementation{Name: "concurrent-stdio-fixture", Version: "1"}, nil)
	session, err := sdk.Connect(lifetime, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		serverSession.Close()
		cancelLifetime()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := serverSession.Close(); err != nil {
			t.Error("server session did not close cleanly", err)
		}
		if err := session.Close(); err != nil {
			t.Error("client session did not close cleanly", err)
		}
		cancelLifetime()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	})
	waitCtx, cancelWaits := context.WithCancel(t.Context())
	done := make(chan error, parallel)
	joined := false
	join := func() {
		if joined {
			return
		}
		joined = true
		cancelWaits()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for i := 0; i < parallel; i++ {
			select {
			case <-done:
			case <-deadline.C:
				t.Error("SDK wait cancellation did not join all calls")
				return
			}
		}
		for i := 0; i < parallel; i++ {
			select {
			case <-detached:
			case <-deadline.C:
				t.Error("cancelled HTTP wait did not detach")
				return
			}
		}
	}
	t.Cleanup(join)
	for i := 0; i < parallel; i++ {
		id := fmt.Sprintf("review_operation_%032x", i+1)
		go func() {
			_, err := session.CallTool(waitCtx, &mcp.CallToolParams{Name: "review_operation_read", Arguments: map[string]any{"operation_id": id, "wait_seconds": 50}})
			done <- err
		}()
	}
	seen := map[string]bool{}
	startDeadline := time.NewTimer(3 * time.Second)
	defer startDeadline.Stop()
	for i := 0; i < parallel; i++ {
		select {
		case id := <-started:
			if seen[id] {
				t.Fatal("review read duplicated")
			}
			seen[id] = true
		case <-startDeadline.C:
			t.Fatal("four review waits did not reach HTTP concurrently")
		}
	}
	if waiting.Load() != parallel {
		t.Fatal("review waits ended before concurrent proof")
	}
	invoke := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("unrelated %s did not finish while waits remained blocked: %v", name, err)
		}
		if waiting.Load() != parallel {
			t.Fatal("unrelated tool completed only after releasing waits")
		}
		return result
	}
	var status client.ReviewOperationStatusOutput
	decodeResult(t, invoke("review_operation_read", map[string]any{"operation_id": fmt.Sprintf("review_operation_%032x", 99), "wait_seconds": 0}), &status)
	if status.State != "queued" || immediate.Load() != 1 {
		t.Fatal("wait-zero read changed or retried")
	}
	send := map[string]any{"recipient_worker_id": f.recipient, "body": body, "idempotency_key": "concurrent-original-message"}
	var first, replay client.MessageReceipt
	decodeResult(t, invoke("inbox_send", send), &first)
	decodeResult(t, invoke("inbox_send", send), &replay)
	if first.ID == "" || first != replay || mutations.Load() != 2 {
		t.Fatal("explicit replay changed receipt or request count")
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM inbox_messages WHERE project_id=? AND sender_worker_id=? AND recipient_worker_id=? AND body=?`, f.project, f.sender, f.recipient, body).Scan(&count); err != nil || count != 1 {
		t.Fatal("message effect was not exactly once", count, err)
	}
	if waitCalls.Load() != parallel || f.providers.Load() != 1 {
		t.Fatal("read repeated or fixture performed additional provider work")
	}
	join()
	if waiting.Load() != 0 {
		t.Fatal("cancelled waits retained HTTP ownership")
	}
	t.Logf("same stdio SDK session: %d blocked waits, 1 wait-zero HTTP read, 2 explicit mutation calls, 1 message effect; all waits cancelled and joined", parallel)
}
