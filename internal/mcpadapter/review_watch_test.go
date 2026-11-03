package mcpadapter_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewWatchSDKActualSnapshotAttachRaceQuietTerminalAndCancellation(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	snapshotRead, releaseSnapshot := make(chan struct{}), make(chan struct{})
	waitEntered, waitExited := make(chan struct{}, 4), make(chan struct{}, 4)
	var once sync.Once
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		watch := strings.HasSuffix(r.URL.Path, "/reviews/watch")
		if watch && r.URL.Query().Get("after") == "" {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			once.Do(func() {
				close(snapshotRead)
				select {
				case <-releaseSnapshot:
				case <-r.Context().Done():
				}
			})
			for k, values := range recorded.Header() {
				for _, v := range values {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		if watch {
			waitEntered <- struct{}{}
		}
		handler.ServeHTTP(w, r)
		if watch {
			waitExited <- struct{}{}
		}
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	providers := f.providers.Load()
	var snapshot client.ReviewWatchResult
	doneSnapshot := make(chan *mcp.CallToolResult, 1)
	go func() {
		v, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_reviews", Arguments: map[string]any{"channel": "cli", "limit": 1}})
		if err != nil {
			t.Error(err)
		}
		doneSnapshot <- v
	}()
	select {
	case <-snapshotRead:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not read")
	}
	var queued client.ReviewRequestReceipt
	args := executionArguments()
	args["wait_seconds"] = 0
	decodeResult(t, call(t, session, "review_request", args), &queued)
	close(releaseSnapshot)
	decodeResult(t, <-doneSnapshot, &snapshot)
	if snapshot.Snapshot == nil || len(snapshot.Snapshot.Items) != 1 || snapshot.Snapshot.Items[0].ID == queued.ID || snapshot.EventCursor == "" {
		t.Fatal("snapshot mixed later commit", snapshot)
	}
	var changes client.ReviewWatchResult
	decodeResult(t, call(t, session, "wait_for_reviews", map[string]any{"channel": "cli", "after": snapshot.EventCursor, "wait_seconds": 0}), &changes)
	if len(changes.Changes) != 1 || changes.Changes[0].Operation.ID != queued.ID {
		t.Fatal("snapshot/attach race lost request", changes)
	}
	<-waitEntered
	<-waitExited
	waitArgs := map[string]any{"channel": "cli", "after": changes.EventCursor, "wait_seconds": 900}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		v, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_reviews", Arguments: waitArgs})
		if err != nil {
			t.Error(err)
		}
		done <- v
	}()
	select {
	case <-waitEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("quiet wait not entered")
	}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "fixture"}, "job.updated", "job-unrelated", 1, struct{}{})
	}); err != nil {
		t.Fatal(err)
	}
	var execution client.ReviewExecutionOutput
	decodeResult(t, call(t, session, "review_execution_read", nil), &execution)
	if execution.Revision != 1 || execution.Scheduling.Revision != 0 || execution.Scheduling.AutomaticHosted || execution.Scheduling.ProjectID != f.project {
		t.Fatal("execution lost independent scheduling", execution)
	}
	select {
	case v := <-done:
		t.Fatal("unrelated activity ended wait", v)
	case <-time.After(30 * time.Millisecond):
	}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason='fixture_hold',revision=revision+1 WHERE id=?`, queued.ID); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "fixture"}, "review.operation_updated", queued.ID, 2, struct{}{})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-done:
		decodeResult(t, v, &changes)
	case <-time.After(2 * time.Second):
		t.Fatal("terminal hold did not wake watch")
	}
	if len(changes.Changes) != 1 || changes.Changes[0].Operation.State != "held" {
		t.Fatal(changes)
	}
	<-waitExited
	waitArgs["after"] = changes.EventCursor
	cancelCtx, cancelWait := context.WithCancel(t.Context())
	defer cancelWait()
	cancelled := make(chan error, 1)
	go func() {
		_, err := session.CallTool(cancelCtx, &mcp.CallToolParams{Name: "wait_for_reviews", Arguments: waitArgs})
		cancelled <- err
	}()
	<-waitEntered
	cancelWait()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK wait not cancelled")
	}
	select {
	case <-waitExited:
	case <-time.After(2 * time.Second):
		t.Fatal("server wait not released")
	}
	if f.providers.Load() != providers {
		t.Fatal("watch performed provider work")
	}
	for _, bad := range []map[string]any{{"limit": 0}, {"after": ""}, {"pull_number": 0}, {"channel": "other"}, {"wait_seconds": 901}, {"after": nil}, {"cursor": "pagination"}} {
		errorResult(t, call(t, session, "wait_for_reviews", bad), "invalid_arguments")
	}
}

func TestReviewWatchSDKOfflineDiscoveryAndInvalidArguments(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": c}, "absent-private-token"), "alpha")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name != "wait_for_reviews" {
			continue
		}
		found = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatal("watch annotation implies provider work")
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
			Additional bool                       `json:"additionalProperties"`
		}
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != 5 || len(schema.Required) != 0 || schema.Additional {
			t.Fatal(string(raw))
		}
		var wait struct{ Default, Maximum int }
		if json.Unmarshal(schema.Properties["wait_seconds"], &wait) != nil || wait.Default != 300 || wait.Maximum != 900 {
			t.Fatal("watch bounds not discoverable", string(raw))
		}
	}
	if !found {
		t.Fatal("watch tool not advertised")
	}
	for _, bad := range []any{map[string]any{"limit": 0}, map[string]any{"after": ""}, map[string]any{"wait_seconds": 901}, map[string]any{"project_id": "foreign"}, json.RawMessage(`{"limit":null}`), json.RawMessage(`{"limit":1,"limit":2}`), json.RawMessage(`{"After":"wrong-case"}`)} {
		errorResult(t, call(t, session, "wait_for_reviews", bad), "invalid_arguments")
	}
	if requests.Load() != 0 {
		t.Fatal("discovery/invalid arguments read credentials or contacted service")
	}
}
