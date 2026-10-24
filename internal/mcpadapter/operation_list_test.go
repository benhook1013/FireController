package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestOperationListSDKDiscoveryStrictArgumentsBeforeCredentials(t *testing.T) {
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
		if tool.Name != "review_operations_list" {
			continue
		}
		found = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatal("untruthful list annotations")
		}
		data, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		if json.Unmarshal(data, &schema) != nil || len(schema.Properties) != 5 || len(schema.Required) != 0 || schema.Additional {
			t.Fatal("operation list schema changed", string(data))
		}
		for _, name := range []string{"pull_number", "channel", "status", "limit", "cursor"} {
			if schema.Properties[name] == nil {
				t.Fatal("missing list field", name)
			}
		}
	}
	if !found {
		t.Fatal("operation list missing from SDK discovery")
	}
	for _, args := range []any{map[string]any{"pull_number": 0}, map[string]any{"pull_number": 2147483648}, map[string]any{"pull_number": 1.5}, map[string]any{"channel": "other"}, map[string]any{"channel": ""}, map[string]any{"status": "completed"}, map[string]any{"status": ""}, map[string]any{"limit": 0}, map[string]any{"limit": 101}, map[string]any{"limit": nil}, map[string]any{"cursor": ""}, map[string]any{"cursor": " bad"}, map[string]any{"profile": "other"}, map[string]any{"project_id": "other"}, json.RawMessage(`{"status":"all","status":"pending"}`), json.RawMessage(`{"Status":"all"}`)} {
		errorResult(t, call(t, session, "review_operations_list", args), "invalid_arguments")
	}
	if requests.Load() != 0 {
		t.Fatal("discovery or invalid input contacted service")
	}
}

func TestOperationListSDKRealHandlerKeysetRecoveryAndRevocation(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var lists atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/operations") {
			lists.Add(1)
			if r.Method != "GET" || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("operation list lost binding")
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	providers := f.providers.Load()
	var queued client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", executionArguments()), &queued)
	var pending client.ReviewOperationsPage
	decodeResult(t, call(t, session, "review_operations_list", map[string]any{}), &pending)
	if len(pending.Items) != 1 || pending.Items[0].ID != queued.ID || pending.HasMore {
		t.Fatal(pending)
	}
	args := map[string]any{"status": "all", "pull_number": 2, "channel": "cli", "limit": 1}
	var first, next client.ReviewOperationsPage
	decodeResult(t, call(t, session, "review_operations_list", args), &first)
	if len(first.Items) != 1 || !first.HasMore || first.Cursor == "" {
		t.Fatal(first)
	}
	args["cursor"] = first.Cursor
	decodeResult(t, call(t, session, "review_operations_list", args), &next)
	if len(next.Items) != 1 || next.Items[0].ID == first.Items[0].ID || next.HasMore || next.Cursor != "" {
		t.Fatal(next)
	}
	args["status"] = "pending"
	errorResult(t, call(t, session, "review_operations_list", args), "validation_failed")
	if lists.Load() != 4 || f.providers.Load() != providers {
		t.Fatal("list paginated, retried or contacted provider", lists.Load())
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	errorResult(t, call(t, session, "review_operations_list", map[string]any{}), "forbidden")
}
